// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/alert"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/panel"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/reqlog"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// checkinReportFn 把 scheduler 的**进程内**签到接到 HTTP 入口（POST /v1/checkin）。
//
// ★ 直接调 CheckinAll，而不是 exec deploy/signin ★ 号池的 credits 只由 CheckinAll
// 这条路径写入（SetCreditsDetailed）；外部 CLI 虽然能签到成功，但那是另一个进程，
// 网关内存里的额度不会更新，控制台照样显示旧值 —— 这正是 2026-09-22 用户报的
// 「额度没刷新」。详见 internal/server/checkin.go。
func checkinReportFn(sch *scheduler.Scheduler, p *pool.Pool, cfg *Config) func() (server.CheckinReport, bool, error) {
	return func() (server.CheckinReport, bool, error) {
		outcomes, err := sch.CheckinAll()
		if errors.Is(err, scheduler.ErrBusy) {
			// 手动入口与定时撞车：不是错误，交给 handler 回 429 busy。
			return server.CheckinReport{}, true, nil
		}
		if err != nil {
			return server.CheckinReport{}, false, err
		}
		// realm 由号池现查（CheckinOutcome 本身不带 realm，免得 scheduler 与 pool
		// 两处各存一份口径）；查不到留空，前端按「—」显示。
		return buildCheckinReport(outcomes, realmOfFn(p),
			cfg.Schedule.CheckinEnabled, cfg.Schedule.CheckinHours), false, nil
	}
}

// realmOfFn 返回「按 uid 现查 realm」的闭包。手动回执与签到历史落盘共用它，
// 保证两条路径的 realm 归属永远一致。
func realmOfFn(p *pool.Pool) func(string) string {
	return func(uid string) string {
		if a := p.AuthByUID(uid); a != nil {
			return a.Realm()
		}
		return ""
	}
}

// buildCheckinReport 把 scheduler 的结果映射成 HTTP 响应体（纯函数，便于单测：
// 计数与 realm 归属错了不会报错，只会让面板显示错，必须锁住）。
func buildCheckinReport(outcomes []scheduler.CheckinOutcome, realmOf func(string) string,
	enabled bool, hours []int) server.CheckinReport {
	rep := server.CheckinReport{
		Enabled: enabled,
		Hours:   hours,
		Total:   len(outcomes),
		Results: make([]server.CheckinResult, 0, len(outcomes)),
	}
	for _, o := range outcomes {
		rep.Results = append(rep.Results, server.CheckinResult{
			UID:      o.UID,
			Nickname: o.Nickname,
			Realm:    realmOf(o.UID),
			Status:   string(o.Status),
			Credits:  o.Credits,
			Detail:   o.Detail,
		})
		switch o.Status {
		case scheduler.CheckinOK:
			rep.OK++
		case scheduler.CheckinAlready:
			rep.Already++
		case scheduler.CheckinFail:
			rep.Fail++
		case scheduler.CheckinSkipped:
			rep.Skipped++
		}
	}
	return rep
}

// modelJSONPath 由 state.json 路径推导 model.json 路径（同目录同名换缀）：
// 两者同为数据目录持久化物（Docker ./data volume），配套而非各自配置。
// state 路径为空（纯内存测试形态）→ 空 = 禁用 model.json 落盘（内存 + 种子仍可用）。
func modelJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "model.json")
}

// checkinJSONPath 由 state.json 路径推导 checkin.json 路径（同目录同名换缀）：
// 签到历史与 state.json 同为数据目录持久化物（Docker ./data volume），配套而非各自配置。
// 空 state 路径（纯内存测试形态）→ 空 = 禁用落盘（历史仅存内存）。
func checkinJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "checkin.json")
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	hashPwd := flag.String("hash-password", "", "print PBKDF2 hash for panel.password_hash and exit")
	flag.Parse()
	if *hashPwd != "" {
		fmt.Println(panel.HashPassword(*hashPwd))
		return
	}

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// model.json 本地缓存接线（context_length 四级查找链第 3 级）：数据目录与
	// state.json 同风格（Docker volume 持久化路径 ./data）。首次缺失/损坏自动回落
	// 仓库种子 embed；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(modelJSONPath(cfg.StateFile))

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// auths 目录热加载：新增凭证文件自动进池，免去「加完账号手动重启网关」。
	// 启动时的 SyncToDir 已建立基线，监听只在后续目录内容变化时触发（见 pool/watch.go）。
	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	defer stopWatch()

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	// 连败降权（issue #114）：ErrClient/传输层连败 N 次临时出池。
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域在途分档（WAF 403 修复 P1-1，默认 2）
	p.SetSoftRateMax(cfg.SoftRateMaxDur)               // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	// 积分耗尽后的冷却时长（cooldown.credit_exhausted，issue #78）：0 = 保持
	// 「硬冷却到次日 04:00」旧行为；配了就只退出这一小段再回候选池。
	p.SetCreditExhausted(cfg.CreditExhaustedDur)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)         // 最早到期优先路由（panel prefer_expiring，默认 true）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	// 账号级流量整形 + 全池并发闸（config pool.weight_default 等，默认无此功能）。
	// 必须与既有 entry 重算生效值（SetThrottleDefaults 内部遍历 byUID 重算）。
	p.SetThrottleDefaults(cfg.Pool.WeightDefault, cfg.Pool.MaxConcurrencyDefault, cfg.Pool.MinIntervalDefault, cfg.Pool.MaxTotalConcurrency)
	// 11140 账号级风控处置（pool.account_fault_hold / disable_after；默认 24h / 3 次）。
	p.SetAccountFaultPolicy(cfg.AccountFaultHoldDur, cfg.Pool.AccountFaultDisableAfter)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 额外根证书（upstream.ca_bundle，企业代理/Clash TLS 解密）：启动期加载，失败即退出。
	if p := strings.TrimSpace(cfg.Upstream.CABundle); p != "" {
		if err := up.SetCABundle(p); err != nil {
			log.Fatalf("upstream.ca_bundle: %v", err)
		}
	}
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
		// 让「按账号线路」的 transport 也继承这份配置（连接池参数 / 首字节超时）。
		upstream.ConfigureBaseTransport(tr)
	}
	// 账号级出口线路表（config.json 的 proxies 段 / WB2A_PROXIES）：
	// 账号文件顶层的 `proxy` 写线路名，这里注入「线路名 → 代理地址」映射。
	// 未绑线路的账号行为不变；绑了未配置的线路名时明确报错，不静默直连。
	upstream.ConfigureProxies(cfg.Proxies)
	if names := upstream.RouteNames(); len(names) > 0 {
		log.Printf("出口线路已加载：%d 条（%s）—— 账号绑定的线路名按它解析",
			len(names), strings.Join(names, ", "))
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled
	// 国内版对话上游覆盖（upstream.cn_chat_base，空 = 内置默认 copilot.tencent.com）。
	if v := strings.TrimSpace(cfg.Upstream.CNChatBase); v != "" {
		up.ChatBaseCN = v
	}
	// 聊天 SSE 同账号瞬时重试（upstream.retry_*，默认关 = 行为逐字一致）。
	up.Retry = upstream.RetryConfig{
		Enabled:  cfg.Upstream.RetryEnabled,
		Attempts: cfg.Upstream.RetryAttempts,
		Backoff:  time.Duration(cfg.Upstream.RetryBackoff * float64(time.Second)),
	}

	// 积分保底（panel）：触底号不接实测收费模型；目录倍率兑底堵「无观测=放行」。
	p.SetCreditFloor(cfg.Pool.CreditFloor)
	p.SetModelRateOf(up.ModelRate)

	// 签到历史落盘（GET /v1/checkin/history 的数据源）。此前签到结果只活在
	// POST /v1/checkin 的 HTTP 响应里——刷新页面即失忆，自动 9/21 那趟更是完全
	// 看不到（日志只有一行聚合计数，成功路径不打日志）。文件缺失 = 还没有历史
	// （不报错）；解析失败改名留证再从空开始，绝不静默覆盖。
	checkinStore := server.NewCheckinHistoryStore(checkinJSONPath(cfg.StateFile))
	checkinStore.Load()

	// scheduleSeed 部署私有盐：让 random_delay_minutes 的每日随机时刻在重启后稳定、
	// 且不同部署错开（上游用 APIKey 或 AuthDir 兜底——两者都已具备、无需新增熵源；
	// 绝不出现在日志里）。
	scheduleSeed := cfg.APIKey
	if scheduleSeed == "" {
		scheduleSeed = cfg.AuthDir
	}
	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		SchoolHours:         cfg.Schedule.SchoolHours,
		CatHours:            cfg.Schedule.CatHours,
		GrowthHours:         cfg.Schedule.GrowthHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		// 触发时刻抖动窗口（schedule.jitter_minutes，0 = 精确整点 = 旧行为）。
		JitterMinutes: cfg.Schedule.JitterMinutes,
		// 实例盐（schedule.jitter_salt，缺省空 = 与引入前逐字一致）。
		JitterSalt: cfg.Schedule.JitterSalt,
		// 账号间随机抖动 + 档位小时摊开（schedule.account_delay_* / random_delay_minutes）。
		AccountDelayMinSeconds: cfg.Schedule.AccountDelayMinSeconds,
		AccountDelayMaxSeconds: cfg.Schedule.AccountDelayMaxSeconds,
		RandomDelayMinutes:     cfg.Schedule.RandomDelayMinutes,
		RandomDelaySeed:        scheduleSeed,
		ExpiringSoonWindow:     cfg.ExpiringSoonDur, // 快过期积分优先消耗（issue:积分过期）
		// 任务执行台账与当日失败重试（schedule.ledger_file / retry_*）。
		// 重试默认关闭（RetryDelayMinutes=0），台账恒在（纯内存，除非给了落盘路径）。
		LedgerFile:        cfg.Schedule.LedgerFile,
		RetryDelayMinutes: cfg.Schedule.RetryDelayMinutes,
		RetryMaxPerDay:    cfg.Schedule.RetryMaxPerDay,
		CheckinDisabled:   !cfg.Schedule.CheckinEnabled,
		TravelDisabled:    !cfg.Schedule.TravelEnabled,
		ActivityDisabled:  !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled: !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:    !cfg.Schedule.SchoolEnabled,
		CatDisabled:       !cfg.Schedule.CatEnabled,
		GrowthDisabled:    !cfg.Schedule.GrowthEnabled,
		// 保号类任务是否覆盖禁用账号（panel 语义：禁用只关选号，不停保号）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
		// 签到结束即落盘：手动入口与定时排程都走 CheckinAll，挂钩一处两条路径全覆盖。
		// 映射成 HTTP 口径复用 buildCheckinReport —— 保证「即时回执」与「历史回放」
		// 永远是同一份数据，不会出现两套计数口径。
		OnCheckinDone: func(outcomes []scheduler.CheckinOutcome, started, finished time.Time) {
			checkinStore.Append(
				buildCheckinReport(outcomes, realmOfFn(p), cfg.Schedule.CheckinEnabled, cfg.Schedule.CheckinHours),
				started, finished)
		},
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（Go 原生：BlackcatNeed → RunNightChats）", cfg.Schedule.CatHours)
	}
	if cfg.Schedule.JitterMinutes > 0 {
		log.Printf("排程抖动已启用：各任务触发时刻在名义整点后 0-%d 分钟内确定性偏移（schedule.jitter_minutes）",
			cfg.Schedule.JitterMinutes)
	}
	// 任务执行台账与当日失败重试（schedule.ledger_file / retry_*）。
	// 台账恒在（内存），落盘与否取决于 ledger_file；重试默认关闭。
	if cfg.Schedule.LedgerFile != "" {
		log.Printf("任务执行台账已落盘：%s（每类任务一轮执行后覆写，重启后仍可对账；实时视图见 /status 的 task_ledger）",
			cfg.Schedule.LedgerFile)
	} else {
		log.Printf("任务执行台账仅在内存（schedule.ledger_file 为空，重启后只剩新跑过的记录）")
	}
	if cfg.Schedule.RetryDelayMinutes > 0 && cfg.Schedule.RetryMaxPerDay > 0 {
		log.Printf("当日失败重试已启用：某类任务一轮「全灭」（有失败且无任何账号做成）后 %d 分钟补跑，每类每日最多 %d 次（schedule.retry_delay_minutes / retry_max_per_day）",
			cfg.Schedule.RetryDelayMinutes, cfg.Schedule.RetryMaxPerDay)
	} else {
		log.Printf("当日失败重试已关闭（schedule.retry_delay_minutes=%d retry_max_per_day=%d；全灭只记 WARN 与台账，不自动补跑）",
			cfg.Schedule.RetryDelayMinutes, cfg.Schedule.RetryMaxPerDay)
	}

	// 管理操作审计（config admin.audit_enabled，默认关闭）：把 /admin 下每个动作
	// 追加一行 JSONL 到磁盘。构造期做可写性预检并 fail-fast——路径不可写是审计最
	// 常见的失效原因，且完全能在启动时发现；放到第一次管理操作才暴露，等于把一次
	// 「配置错」推迟成「真出事时才发现审计是空的」，那正是审计最没用的时刻。
	var auditLog *server.AuditLog
	if cfg.Admin.AuditEnabled {
		al, err := server.NewAuditLog(cfg.Admin.AuditFile, cfg.APIKey)
		if err != nil {
			log.Fatalf("管理操作审计初始化失败：%v", err)
		}
		auditLog = al
		log.Printf("管理操作审计已启用：%s（每个 /admin 动作追加一行 JSONL）", al.Path())
	}

	// 当日积分预算闸（budget.daily_credit_limit，默认关闭）。
	if cfg.Budget.DailyCreditLimit > 0 {
		log.Printf("当日积分预算已启用：累计扣费达 %.2f credit 后拒服务（按 CST 自然日重置，实时用量见 /status 的 daily_budget）",
			cfg.Budget.DailyCreditLimit)
	}

	// —— panel 能力装配（吸收自 linguo2625469/workbuddy2api-panel）——
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	usagePath := stateSibling(cfg.StateFile, "usage.json")
	usageRec := usage.New(usagePath)
	usageRec.Start()
	defer usageRec.Stop()

	// 请求指标 + 脱敏 JSONL 归档（logging.*；归档默认关闭，只跑进程内指标）。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()

	// 运行期可变配置快照（面板在线改 api_key / soft_rate / 脱敏 / 来源记录时热生效）。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
	})

	pn := panel.New(panel.Config{
		Pool:        p,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Usage:       usageRec,
		RequestLog:  requestLog,
		ProbeFile:   stateSibling(cfg.StateFile, "output_probes.json"),
		// 面板独立密码（panel.password_hash，PBKDF2）+ 会话有效期。
		PasswordHash: cfg.Panel.PasswordHash,
		SessionTTL:   time.Duration(cfg.Panel.SessionHours) * time.Hour,
		Live:         live,
		ConfigPath:   *cfgPath,
		LoadConfig:   func() (any, error) { return Load(*cfgPath) },
		SaveConfig: func(raw []byte, write bool) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch, write)
		},
	})
	// 成长任务队列（面板任务中心同管线）：独立排程（schedule.growth_hours，默认
	// 01:00），到点由 scheduler.dispatch 调用；回调内部异步启动，不等执行完。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	// 面板日志镜像（吸收自 panel）：log 输出（stderr）与聊天表格日志（stdout）
	// 同时写进环形缓冲，/panel/ 日志页可见；环形缓冲有界，不额外占盘。
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		AuthKeys:     buildAuthKeys(cfg),
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		// 单请求体上限（server.max_body_mb，默认 16MB；0 = 不限）。
		MaxBodyBytes: int64(cfg.Server.MaxBodyMB) << 20,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		// 模型别名/重定向（upstream.model_aliases，默认空 = 不启用）。
		ModelAliases: cfg.Upstream.ModelAliases,
		// panel 能力：管理面板（/panel/）+ 逐请求用量记录（吸收自 panel 分支）。
		Panel:      pn,
		Usage:      usageRec,
		RequestLog: requestLog,
		Live:       live,
		// 请求日志是否记录调用来源（IP/UA）；经 livecfg 热生效，静态值供 Live 为 nil 的裸用路径。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// 运维管理端点开关（config admin.enabled，默认 false）。
		AdminEnabled: cfg.Admin.Enabled,
		// Prometheus 指标端点开关（config metrics.enabled，默认 false）。
		MetricsEnabled: cfg.Metrics.Enabled,
		// 手动任务触发（admin.enabled 下的 /admin/tasks/{name}/run）：把调度器
		// 作为 TaskRunner 注入，server 包不必反向 import scheduler。
		Tasks: sch,
		// 管理操作审计接收器（admin.audit_enabled，默认关闭时为零值 nil =
		// 不审计、零开销）。
		Audit: auditLog,
		// 当日积分预算上限（budget.daily_credit_limit，0 = 关闭该闸）。
		BudgetLimit: cfg.Budget.DailyCreditLimit,
		// 单账号当日 credit 上限（pool.account_daily_credit_limit，0 = 关闭）。
		AccountDailyCreditLimit: cfg.Pool.AccountDailyCreditLimit,
		// 任务执行台账只读视图（/status 的 task_ledger 段 + /metrics 的任务指标）。
		// 与 Tasks 同款：*taskledger.Store 结构上即满足 server 侧的窄接口，
		// server 包不必反向 import scheduler。
		TaskLedger: sch.Ledger(),
		// 手动签到入口（POST /v1/checkin）。★ 走进程内 CheckinAll ★ 外部 CLI
		// 签到不会更新网关内存额度（见 internal/server/checkin.go 顶部注释）。
		CheckinFn: checkinReportFn(sch, p, cfg),
		// 签到历史（GET /v1/checkin/history）与排程快照（下一个自动签到时点，
		// 含 jitter 派生）。*scheduler.Scheduler 同时实现 CheckinScheduleProvider。
		CheckinHistory:  checkinStore,
		CheckinSchedule: sch,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, balanceIntervalFor(cfg)) // 余额后台刷新（schedule.balance_refresh_*）

	// 可用性阈值告警（config alerting.enabled，默认关闭）：独立 ticker 评估只读健康
	// 快照，越界时 POST 到运维自备的 webhook。与请求路径完全隔离，且从不调用上游。
	alertMon := alert.New(alert.Config{
		Enabled:             cfg.Alerting.Enabled,
		WebhookURL:          cfg.Alerting.WebhookURL,
		Secret:              cfg.Alerting.Secret,
		Interval:            time.Duration(cfg.Alerting.IntervalSeconds) * time.Second,
		Timeout:             time.Duration(cfg.Alerting.TimeoutSeconds) * time.Second,
		StartupGrace:        time.Duration(cfg.Alerting.StartupGraceSeconds) * time.Second,
		MinHealthyCN:        cfg.Alerting.MinHealthyCN,
		MinHealthyGlobal:    cfg.Alerting.MinHealthyGlobal,
		RecoverHealthy:      cfg.Alerting.RecoverHealthy,
		BreakerThreshold:    cfg.Alerting.BreakerThreshold,
		MinConsecutiveFails: cfg.Alerting.MinConsecutiveFails,
		ForTicks:            cfg.Alerting.ForTicks,
		ClearTicks:          cfg.Alerting.ClearTicks,
		SendResolve:         cfg.Alerting.SendResolve,
		ServiceName:         server.ServiceName,
	}, alertSource{pool: p, h: h})
	go alertMon.Run(ctx)
	defer alertMon.Stop()
	if !cfg.Alerting.Enabled {
		log.Printf("可用性告警已禁用（alerting.enabled=false）")
	} else {
		log.Printf("可用性告警已启用：每 %ds 评估，健康阈值 cn=%d / global=%d（0=关闭该规则），熔断阈值=%d / 连败阈值=%d（0=关闭）",
			cfg.Alerting.IntervalSeconds, cfg.Alerting.MinHealthyCN,
			cfg.Alerting.MinHealthyGlobal, cfg.Alerting.BreakerThreshold, cfg.Alerting.MinConsecutiveFails)
	}

	warnInsecureListen(cfg.Listen, cfg.APIKey)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// 吸收自 panel（issue #100）：server.read_timeout 配置化，默认 300s，
		// "0" = 不限制。旧 max_body_mb 移除后请求体无上限，超大 body 成为唯一
		// 的自然约束——超时会得到连接错误（read timeout）而非 413。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径回落当前目录）。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// balanceIntervalFor 由 schedule.balance_refresh_* 推出余额后台刷新间隔（0 = 关闭）。
func balanceIntervalFor(c *Config) time.Duration {
	if !c.Schedule.BalanceRefreshEnabled {
		return 0
	}
	m := c.Schedule.BalanceRefreshMinutes
	if m <= 0 {
		m = 5
	}
	return time.Duration(m) * time.Minute
}

// restartRequiredFields 返回无法热生效、需重启进程的字段名（面板据此提示）。
// restartRequiredPaths 这些字段只在**启动时**读取，改了必须重启才生效。
var restartRequiredPaths = []string{
	"listen", "auth_dir", "state_file", "server.read_timeout", "server.max_body_mb",
	"upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds",
	"upstream.user_agent",
	"upstash", "session_sticky.ttl", "session_sticky.gc_interval",
	"logging.request_archive_enabled", "logging.request_retention_days", "logging.request_archive_max_mb",
	"panel.password_hash", "panel.session_hours",
}

// applyIncoming 深合并提交的配置，并返回「确实变了、且必须重启才生效」的字段。
//
// **必须在这里做深拷贝**：`mergeConfigMaps` 是**原地深合并**（直接改 cur），
// 合并后再拿 cur 去比就永远相等 —— restart_required 会恒为空，看起来像
// 「所有字段都热生效了」，而实际上改 listen 也没人提醒你重启。
func applyIncoming(cur, incoming map[string]any) (map[string]any, []string) {
	before := deepCopyMap(cur)
	merged := mergeConfigMaps(cur, incoming)
	if before == nil {
		// 深拷贝失败：宁可保守地报告「这些字段可能需要重启」，也不要因为
		// before 与 merged 变成同一个对象而给出「全部热生效」的假象。
		return merged, append([]string(nil), restartRequiredPaths...)
	}
	return merged, restartRequiredFields(before, merged)
}

// deepCopyMap 经 JSON 往返深拷贝（配置是小对象，代价可忽略）。
func deepCopyMap(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// restartRequiredFields 返回「**本次确实变化了**、且必须重启才生效」的字段。
//
// 此前这里返回的是**固定清单**：只要保存一次就报全部 17 个字段，哪怕只改了
// schedule（那是热生效的）。后果不是多一行字 —— 用户会被引导去做一次没必要的
// 重启，久而久之就学会忽略这条提示，而它真正该被重视的时候（改了 listen）
// 也就不会被重视了。
func restartRequiredFields(oldCfg, newCfg map[string]any) []string {
	out := make([]string, 0, len(restartRequiredPaths))
	for _, path := range restartRequiredPaths {
		if !reflect.DeepEqual(lookupPath(oldCfg, path), lookupPath(newCfg, path)) {
			out = append(out, path)
		}
	}
	return out
}

// lookupPath 按 "a.b.c" 取值；中间缺失返回 nil（与「值为 null」同等对待）。
func lookupPath(m map[string]any, path string) any {
	var cur any = m
	for _, key := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[key]
	}
	return cur
}

// saveConfig 面板保存配置：深合并 → 校验 → 原子落盘 → 热应用，返回需重启字段清单。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, write bool) ([]string, error) {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged, restartFields := applyIncoming(cur, incoming)
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	// write=false：只校验并热应用，不落盘。
	//
	// 外部管理端（workbuddy-manager）自己已经在宿主文件系统上原子写好了 config.json；
	// 而上游这里是**单文件 bind mount**，`os.Rename` 覆盖它会返回 EBUSY
	// （device or resource busy）—— 所以写盘权交给调用方，上游只负责校验 + 热应用。
	if write {
		out, err := json.MarshalIndent(merged, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal config: %w", err)
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, out, 0o600); err != nil {
			return nil, fmt.Errorf("write config: %w", err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, fmt.Errorf("replace config: %w", err)
		}
	}
	// 出口线路表：`proxies` 段改了要重新注入，否则内存里的路由表还是旧的 ——
	// 面板那边会显示「已热生效」，而新增/改名的线路实际查不到。
	// ConfigureProxies 同时会清空按线路缓存的 transport，地址变更也能立即生效。
	upstream.ConfigureProxies(newCfg.Proxies)
	if names := upstream.RouteNames(); len(names) > 0 {
		log.Printf("出口线路已重新加载：%d 条（%s）", len(names), strings.Join(names, ", "))
	} else {
		log.Printf("出口线路已清空（config.json 的 proxies 段为空）")
	}

	// 热应用（能立即生效的字段全部应用）。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur)
	p.SetCreditFloor(newCfg.Pool.CreditFloor)
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetThrottleDefaults(newCfg.Pool.WeightDefault, newCfg.Pool.MaxConcurrencyDefault, newCfg.Pool.MinIntervalDefault, newCfg.Pool.MaxTotalConcurrency)
	p.SetAccountFaultPolicy(newCfg.AccountFaultHoldDur, newCfg.Pool.AccountFaultDisableAfter)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours, newCfg.Schedule.ActivityHours,
		newCfg.Schedule.KeepaliveHours, newCfg.Schedule.SchoolHours, newCfg.Schedule.CatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled, !newCfg.Schedule.ActivityEnabled,
		!newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.SchoolEnabled, !newCfg.Schedule.CatEnabled,
		!newCfg.Schedule.GrowthEnabled,
	)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.SetBalanceInterval(balanceIntervalFor(newCfg))
	return restartFields, nil
}

// mergeConfigMaps 把 incoming 深合并进 cur（只覆盖提交的键，保留手写未知键）。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// isLoopbackListen 判断监听地址是否只绑定回环（"127.0.0.1:7863" / "[::1]:7863" /
// "localhost:7863"）。空 host（":7863"）表示所有接口 → 非回环。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// warnInsecureListen 无 api_key 且非回环监听时告警：空 key = 不鉴权，任何可达方
// 都能使用账号池（hub 会首次自动生成 key；本网关只告警，不擅自改用户配置）。
func warnInsecureListen(addr, apiKey string) {
	if apiKey != "" || isLoopbackListen(addr) {
		return
	}
	log.Printf("WARN: api_key 为空且监听 %q 非回环——网关当前不鉴权，任何可达方都能使用账号池；请设置 api_key 或改为 127.0.0.1 监听", addr)
}
