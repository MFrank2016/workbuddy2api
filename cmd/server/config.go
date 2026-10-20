// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/server"
)

// APIKeyEntry 一条**分组密钥**（config 的 api_keys 元素）。
//
// 语义：请求以本 Key 鉴权时，只在与 Groups 有交集的账号中选号（账号侧标签见
// auth.Auth.Groups）。Groups 为空 = 不限分组（等价主密钥语义）。
type APIKeyEntry struct {
	Key    string   `json:"key"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
	// Models 密钥级模型白名单（支持后缀 "*" 通配）；空 = 不限。不匹配本地 400，
	// 零上游请求（吸收自 hub）。
	Models []string `json:"models"`
	// Realm 密钥级域绑定（"cn" / "global"）；空 = 不限。请求路由到的 realm
	// 不匹配时本地 400。
	Realm string `json:"realm"`
}

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权；主密钥，不限分组
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// Proxies 账号级出口线路表：线路名 → 代理地址（http / https / socks5 / socks5h）。
	//
	// 面板会给账号绑一条线路（auths/*.json 顶层 `proxy` 写的是**线路名**），这个名字
	// 在这里解析成地址；线路名查不到时上游**明确报错、不静默直连**（静默直连等于
	// 「以为隔离了其实没隔离」，见 upstream.TransportFor）。
	Proxies map[string]string `json:"proxies"`

	// APIKeys 分组密钥表：每把密钥可绑定若干**业务分组**，请求命中该密钥时只在与
	// 这些分组有交集的账号中选号。
	//
	// 与 api_key 的关系：api_key 是**主密钥**、语义不变（等价 Groups 为空 = 不限分组，
	// 排在本表之前参与匹配）；api_keys 是**纯增量**——老部署不配这一段，行为与改动前
	// 逐字一致（JSON 未知字段对老二进制同样是忽略）。
	//
	// ⚠️ 本表的密钥**不得**写入 api_keys_legacy：那张表是给 nginx 做「旧 key 改写成
	// 当前 key」的别名表。一旦写进去，密钥在入口就被 nginx 改写成主密钥，分组身份当场
	// 丢失，表现为"分组密钥能拿到全部账号"——隔离静默失效，且从网关日志上完全看不出来。
	APIKeys []APIKeyEntry `json:"api_keys"`

	Server struct {
		// ReadTimeout 入站请求读取（含 body 上传）总时长上限（issue #100，吸收自 panel）。
		// http.Server 的 ReadTimeout 覆盖整个请求读取：大上下文/文件块请求经慢链路上传
		// 需要更宽裕的窗口；"0" = 不限制（旧 max_body_mb 的硬编码 60s 约束的接替）。
		ReadTimeout string `json:"read_timeout"` // "300s"；"0" = 不限制
		// MaxBodyMB 单请求体上限（MB，吸收自 hub）：默认 16；0 = 不限制。
		// 网关在读 body 前用 http.MaxBytesReader 拦截，超限 413（防并发大 body 打爆内存）。
		MaxBodyMB int `json:"max_body_mb"`
	} `json:"server"`

	// Logging 请求归档与调用来源（吸收自 panel；归档/来源默认开启，对齐 panel）。
	Logging struct {
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		RequestRetentionDays  int  `json:"request_retention_days"`
		RequestArchiveMaxMB   int  `json:"request_archive_max_mb"`
		RequestClientInfo     bool `json:"request_client_info"`
	} `json:"logging"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	// Admin 运维管理端点开关（issue #138/#118）。默认**关闭**：管理能力默认不暴露，
	// 避免「开了网关就等于开了账号管理面」。开启后
	// /admin/accounts/{uid}/{disable,enable,revive} 可用；鉴权与 /status 同源
	// （withAuth + 同一个 api_key，不另立管理密钥）。
	Admin struct {
		Enabled bool `json:"enabled"` // 默认 false
		// AuditEnabled 管理操作审计日志开关。默认**关闭**：审计会在磁盘上额外
		// 落一份文件，老部署即便开了 admin 也不该被动产生新文件，故与 enabled
		// 拆成两个开关。开启后 /admin 下每个动作（账号 disable/enable/revive、
		// 手动触发任务）追加一行 JSONL，含时间/动作/对象/结果/来源 IP 与
		// api_key 指纹（指纹而非原文）。
		// 要求 admin.enabled 同时开启——审计的对象就是这些端点，端点不存在时
		// 开着审计是个只会误导人的空配置，normalize 直接拒绝。
		AuditEnabled bool `json:"audit_enabled"` // 默认 false
		// AuditFile 审计日志路径。空 = 内置默认 ./data/admin_audit.log。
		// 与 state_file 同约定：父目录由进程自建（NewAuditLog 会 MkdirAll）。
		AuditFile string `json:"audit_file"`
	} `json:"admin"`

	// Metrics Prometheus 指标端点开关。默认**关闭**：开启后 GET /metrics 暴露
	// 进程内只读快照（账号池健康度 + 按模型的请求/延迟/token/积分），供 Prometheus
	// 抓取与 Grafana 看板。与 /status、/v1/stats 同走 withAuth（同一个 api_key，
	// 不另立指标密钥）；抓取端用 authorization/bearer_token 配置即可。
	// 数据源全是进程内状态，scrape 不产生任何上游请求。
	Metrics struct {
		Enabled bool `json:"enabled"` // 默认 false
	} `json:"metrics"`

	// Alerting 可用性阈值告警（webhook 推送）。默认**关闭**：关闭时不起 goroutine、
	// 不发任何 HTTP，行为与改动前逐字一致。目标 URL 由运维自备（自建接收端或
	// 企业微信/钉钉/Slack 的 incoming webhook）；网关从不因此调用上游。
	//
	// 阈值语义：min_healthy_* 是"最低可接受健康账号数"，**低于**该值才告警
	// （min=1 即"健康数为 0 时告警"）；**0 = 关闭该规则**——所以它是哨兵值而非
	// "未设置"，normalize 不会把 0 回落成默认（纯 CN 部署没有 global 账号，
	// min_healthy_global 默认 0 关闭，否则会常驻误报）。
	Alerting struct {
		Enabled             bool   `json:"enabled"`               // 默认 false
		WebhookURL          string `json:"webhook_url"`           // enabled 时必填，scheme 限 http/https
		Secret              string `json:"secret"`                // 非空则对 body 做 HMAC-SHA256 签名
		IntervalSeconds     int    `json:"interval_seconds"`      // 评估周期，默认 30
		TimeoutSeconds      int    `json:"timeout_seconds"`       // webhook 超时，默认 5
		StartupGraceSeconds int    `json:"startup_grace_seconds"` // 启动宽限（避开 auths 未 sync 的 0 健康态），默认 30
		MinHealthyCN        int    `json:"min_healthy_cn"`        // 默认 1；0 = 关闭该规则
		MinHealthyGlobal    int    `json:"min_healthy_global"`    // 默认 0 = 关闭（纯 CN 部署不误报）
		RecoverHealthy      int    `json:"recover_healthy"`       // 恢复阈值（迟滞上沿），默认 1
		BreakerThreshold    int    `json:"breaker_threshold"`     // 熔断账号数阈值，默认 0 = 关闭
		// MinConsecutiveFails 连续失败预警阈值：默认 3；0 = 关闭该规则。
		// 统计「连续失败计数 ≥ 该值」的账号数（熔断计数 fails 口径：401/403/429
		// 等喂入 +1，达 breaker_threshold 熔断），≥1 个即告警。与 breaker_high
		// 的区别：本规则在熔断边界同步发出，语义是「连续失败已达门槛」，而
		// breaker_high 只数已熔断账号。
		MinConsecutiveFails int  `json:"min_consecutive_fails"`
		ForTicks            int  `json:"for_ticks"`    // 连续满足多少拍才触发，默认 2
		ClearTicks          int  `json:"clear_ticks"`  // 连续不满足多少拍才解除，默认 2
		SendResolve         bool `json:"send_resolve"` // 恢复时是否也发通知，默认 false
	} `json:"alerting"`

	// Budget 当日积分预算闸（admission control）。默认**关闭**：
	// daily_credit_limit 为 0 表示不限，行为与改动前逐字一致。
	// 当日累计扣费达到上限后，网关直接拒掉后续对话请求（429 + 明确错误码），
	// 把积分损失截断在阈值附近——防的是跑飞的客户端、忘了关的脚本把积分烧光。
	Budget struct {
		// DailyCreditLimit 当日累计 credit 上限。**0 = 关闭该闸**（不限）——这是
		// 哨兵值而非"未设置"，normalize 不会把它回落成默认值；负值报错。
		// 计数按 CST 自然日重置（上游增长体系按 CST 刷新），进程重启即清零。
		// 只统计上游 usage 给出 credit 的请求，故当日用量是**下界**、真实日耗只会
		// 更多，阈值宜按保守值设（可先把闸当观察模式跑几天，看 /status 的
		// daily_budget.used 再定）。
		DailyCreditLimit float64 `json:"daily_credit_limit"`
	} `json:"budget"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（D5，internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。默认值已对齐官方（A 段变更），用户仍可配完全
		// 自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与白名单
		// 头组 X-IDE-Version 的取值）。空 = 内置默认（对齐官方 5.5.4 分发包）；
		// 显式配置（如升级后的桌面包版本）则随配置走。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（对齐官方内置 CLI
		// 2.137.1）；显式配置则随配置走。
		CliVersion string `json:"cli_version"`

		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底。
		// 容器内无桌面端 Turing SDK，这是把外部生成的 token 注入的入口；空 = 不注入。
		// 每号覆盖优先级：auths 文件 device_token > 本全局值 > DeviceTokenFile（文件兜底）。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
		// 读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略（优雅降级不注入）。
		DeviceTokenFile string `json:"device_token_file"`
		// ClientName 用量归属头 X-Product/X-IDE-Name/X-IDE-Type 的取值。
		// 空（缺省）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
		// 上游用量归因不再出现 client/agentPurpose 为空的网关特征）。
		// 显式配 "SaaS" 还原旧行为（仅 X-Product="SaaS"，不设 X-IDE-*）。
		ClientName string `json:"client_name"`
		// PassthroughIP 是否透传客户端 IP（X-Forwarded-For/X-Real-IP 首段）给上游。
		// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）；true 才透传。
		PassthroughIP bool `json:"passthrough_ip"`
		// CABundle 额外根证书 PEM 文件路径（企业代理/Clash TLS 解密场景）：在系统
		// 根证书之外追加，不关闭证书校验。空 = 不加载。
		CABundle string `json:"ca_bundle"`
		// CNChatBase 国内版对话上游覆盖（如 https://www.workbuddy.cn）；空 = 内置
		// 默认 https://copilot.tencent.com。仅改「往哪发」，计费侧不变。
		CNChatBase string `json:"cn_chat_base"`
		// ModelAliases 模型别名/重定向（吸收自 hub 的后台模型重定向）：键为源模型名
		// （支持后缀 "*" 通配），值为目标模型名；命中后改写请求 model，选号/账本按
		// 目标模型记账。默认空 = 不启用（避免静默改写用户请求）。
		ModelAliases map[string]string `json:"model_aliases"`
		// RetryEnabled 聊天 SSE 请求的**同账号瞬时重试**开关。默认关闭：
		// 打开后仅对 5xx 与传输层失败按 retry_attempts 次重试（同一账号、
		// 指数退避），429/402/401/403 等**永不重试**（配额/凭据问题重试只会更糟）。
		// 关闭 = 行为与现状逐字一致（5xx/传输失败直接走换号轮转）。
		RetryEnabled bool `json:"retry_enabled"`
		// RetryAttempts 同账号最多重试次数（第 1 次失败后额外重试次数），钳 [1,5]。
		// 默认 1：一次瞬时 5xx/连接失败最多多打一遍上游。上限 5 对齐
		// workbuddy-switch-plus mirror.py 的 MAX_ATTEMPTS_CAP。
		RetryAttempts int `json:"retry_attempts"`
		// RetryBackoff 重试退避基数（秒），钳 [0.25,8]。第 n 次重试等待
		// base·2^n 秒封顶 8s（对齐 mirror MAX_BACKOFF_CAP），并施加 ±25% 抖动。
		RetryBackoff float64 `json:"retry_backoff"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer（显式配置仍可覆盖回替换）；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "custom" / "append" / "passthrough"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 403 风控分档），0 = 回落 max_in_flight
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114「累计错误率高/连续失败 N 次的账号移出候选池一段时间」）：
		// ErrClient/传输层这类「不罚号」失败连续计数，达阈临时出池。与冷却/熔断
		// 并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到查余额时，到期时间在此窗口内
		// 的积分被标记为"快过期"，选号优先消耗（issue:积分过期）。空/0 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
		// PreferExpiring 最早到期优先路由开关（panel prefer_expiring，默认 true）：
		// 开启时账号的"快过期积分占比"参与选号权重加成；false = 关闭加成（普通权重）。
		PreferExpiring bool `json:"prefer_expiring"`
		// CreditExhausted 积分耗尽后的冷却时长（如 "10m"）。空 = 旧行为：硬冷却到次日 04:00
		// （等签到恢复）。配了就只退出这一小段，到期回到候选池（issue #78）。
		CreditExhausted string `json:"credit_exhausted"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// WeightDefault 账号级选号权重默认值（weight 乘子）：默认 1.0（等价现行
		// 纯三因子权重，行为逐字一致）。auth 文件显式 weight 的账号用其配置值；
		// 0 = 只读备用（不参与自动轮转）。钳 (0,1000]。
		WeightDefault float64 `json:"weight_default"`
		// MaxConcurrencyDefault 账号级并发上限默认值：默认 0 = 不限（回落现行
		// max_in_flight / max_in_flight_global 分档，行为逐字一致）。auth 文件
		// 显式 max_concurrency 的账号用其配置值。钳 [0,256]。
		MaxConcurrencyDefault int `json:"max_concurrency_default"`
		// MinIntervalDefault 账号级最小请求间隔默认值（秒）：默认 0 = 不限
		// （行为逐字一致）。auth 文件显式 min_interval 的账号用其配置值。
		// 软约束：全部被限住时回落 LRU 兜底，不报「暂无可用账号」。钳 [0,3600]。
		MinIntervalDefault float64 `json:"min_interval_default"`
		// MaxTotalConcurrency 全池总并发上限：默认 0 = 不限（行为逐字一致）。
		// 顶满时本次选号返回 nil → handler 回 503（比 mirror 的忙等让渡更稳，
		// 不进热路径忙等）。钳 [0,512]。
		MaxTotalConcurrency int `json:"max_total_concurrency"`
		// CreditFloor 积分保底：账号余额低于该值时，对实测收费模型不再参与选号
		// （防止收费请求把余额打穿、连免费模型都冷却到次日签到）。0 = 关闭（默认）。
		CreditFloor int64 `json:"credit_floor"`
		// AccountDailyCreditLimit 单账号当日 credit 上限（0 = 关闭）：达到上限的账号
		// 当日不再参与选号（吸收自 hub 的账号限额护栏）。
		AccountDailyCreditLimit float64 `json:"account_daily_credit_limit"`
		// AccountFaultHold 11140 账号级风控处置：长冷却时长（默认 "24h"）；
		// "0" = 立即永久禁用（旧行为）。
		AccountFaultHold string `json:"account_fault_hold"`
		// AccountFaultDisableAfter 连续 N 次 11140 后升级永久禁用（默认 3；<=0 = 不升级）。
		AccountFaultDisableAfter int `json:"account_fault_disable_after"`
	} `json:"pool"`

	// Panel 管理面板鉴权（吸收自 hub 的独立面板密码）：password_hash 非空时启用
	// 密码登录（浏览器会话 cookie），API Key（Bearer）仍然有效；空 = 仅 Bearer（旧行为）。
	Panel struct {
		PasswordHash string `json:"password_hash"` // PBKDF2 摘要（wb2api -hash-password 生成）
		SessionHours int    `json:"session_hours"` // 会话有效期小时数（默认 24）
	} `json:"panel"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	DegradeCooldownDur  time.Duration `json:"-"`
	DegradeCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	ExpiringSoonDur     time.Duration `json:"-"`
	// ServerReadTimeoutDur 解析后的入站请求读取上限（issue #100，吸收自 panel）；
	// 0 = 不限制。
	ServerReadTimeoutDur time.Duration `json:"-"`
	// AccountFaultHoldDur 解析后的 11140 长冷却时长（0 = 立即禁用）。
	AccountFaultHoldDur time.Duration `json:"-"`
	// CreditExhaustedDur 解析后的积分耗尽冷却时长（issue #78）；0 = 不启用
	// （保持「硬冷却到次日 04:00」的旧行为）。
	CreditExhaustedDur time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	// 排程段默认值由 internal/config 集中维护（cmd/server 与 cmd/activity 共用，
	// 消除 issue #49 的默认值漂移）。
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	// 出站指纹默认伪造官方 WorkBuddy 桌面端：UA 三段式 + X-IDE-* 头组
	// （upstream.Client 的 attributionClientName 空值也回落 WorkBuddy，双保险）；
	// 显式 client_name="SaaS" 还原旧行为。
	c.Upstream.ClientName = "WorkBuddy"
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system；显式配置 custom 仍可覆盖回替换
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403
	// 修复 P1-1）；0/负数 normalize 回落。显式 0 需配 -1 之外的方式关闭分档
	// ——这里约定 0 = 未设置回落默认 2（与 max_in_flight 的 0=不限语义不同，
	// 分档键的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	c.Pool.PreferExpiring = true // 缺省开启：奖励积分优先在过期前消耗
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	// Metrics.Enabled / Admin.Enabled 缺省 false（零值）：管理面与指标面都不默认暴露，
	// 老 config 不含这两个键时行为逐字不变。这里显式写出以明示默认值意图。
	c.Metrics.Enabled = false
	// 审计同样缺省关闭；路径给默认值（与 state_file 同目录约定）——开关关着时
	// 该路径是惰性的，写出来只是让 config.example.json 的取值与实现一致。
	c.Admin.AuditEnabled = false
	c.Admin.AuditFile = "./data/admin_audit.log"
	// 预算闸缺省关闭：daily_credit_limit 的 0 是"不限"哨兵，**故意不回落默认值**
	// （与 alerting.min_healthy_global 同风格）。这里显式写出以明示这一意图。
	c.Budget.DailyCreditLimit = 0
	// Alerting 缺省关闭；各阈值/周期给默认值，但 min_healthy_global 与
	// breaker_threshold 的 0 是"关闭该规则"的哨兵，不能改（见 Config.Alerting 注释）。
	c.Alerting.IntervalSeconds = 30
	c.Alerting.TimeoutSeconds = 5
	c.Alerting.StartupGraceSeconds = 30
	c.Alerting.MinHealthyCN = 1
	c.Alerting.MinHealthyGlobal = 0
	c.Alerting.RecoverHealthy = 1
	c.Alerting.BreakerThreshold = 0
	c.Alerting.ForTicks = 2
	c.Alerting.ClearTicks = 2
	// 同号瞬时重试默认关闭：行为与现状逐字一致（字段存疑不影响启动）。
	// RetryAttempts=1（最多重试 1 次）/ RetryBackoff=0.5s 是开启后的保守默认。
	c.Upstream.RetryEnabled = false
	c.Upstream.RetryAttempts = 1
	c.Upstream.RetryBackoff = 0.5
	// 账号级流量整形默认 = 无此功能（weight=1.0 等价纯三因子；限额 0 = 不限）。
	c.Pool.WeightDefault = 1.0
	c.Pool.MaxConcurrencyDefault = 0
	c.Pool.MinIntervalDefault = 0
	c.Pool.MaxTotalConcurrency = 0
	c.Pool.CreditFloor = 0
	// 连续失败预警默认开启（阈值 3，镜像 mirror 的 account_fail_threshold=3）；
	// 0 显式关闭。
	c.Alerting.MinConsecutiveFails = 3
	// 请求归档与调用来源默认开启（对齐 panel）；保留策略默认 7 天 / 100MB。
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestClientInfo = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	// 入站读取上限默认 300s（吸收自 panel issue #100；"0" = 不限制）。
	c.Server.ReadTimeout = "300s"
	// 请求体上限默认 16MB（吸收自 hub）；0 = 不限制。
	c.Server.MaxBodyMB = 16
	// 面板会话有效期默认 24h（密码登录启用时才生效）。
	c.Panel.SessionHours = 24
	// 11140 处置默认：24h 长冷却 + 连续 3 次升级禁用（旧行为可配 "0" 立即禁用）。
	c.Pool.AccountFaultHold = "24h"
	c.Pool.AccountFaultDisableAfter = 3
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_CREDIT_EXHAUSTED"); v != "" {
		c.Pool.CreditExhausted = v
	}
	if v := strings.TrimSpace(os.Getenv("WB2A_PROXIES")); v != "" {
		var table map[string]string
		if err := json.Unmarshal([]byte(v), &table); err == nil {
			c.Proxies = table
		}
	}
	if v := os.Getenv("WB2A_ADMIN_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Admin.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_ADMIN_AUDIT_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Admin.AuditEnabled = b
		}
	}
	if v := os.Getenv("WB2A_ADMIN_AUDIT_FILE"); v != "" {
		c.Admin.AuditFile = v
	}
	if v := os.Getenv("WB2A_BUDGET_DAILY_CREDIT_LIMIT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Budget.DailyCreditLimit = f
		}
	}
	// 排程段的台账与当日失败重试（schedule.*）。三个键都进 env：容器部署改这一组
	// 往往只为「补跑一次」或「把台账落到卷上」，不该逼着用户去改挂载的 config.json。
	if v := os.Getenv("WB2A_SCHEDULE_LEDGER_FILE"); v != "" {
		c.Schedule.LedgerFile = v
	}
	if v := os.Getenv("WB2A_SCHEDULE_RETRY_DELAY_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.RetryDelayMinutes = n
		}
	}
	if v := os.Getenv("WB2A_SCHEDULE_RETRY_MAX_PER_DAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.RetryMaxPerDay = n
		}
	}
	if v := os.Getenv("WB2A_METRICS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Metrics.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_ALERTING_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Alerting.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_ALERTING_WEBHOOK_URL"); v != "" {
		c.Alerting.WebhookURL = v
	}
	if v := os.Getenv("WB2A_ALERTING_SECRET"); v != "" {
		c.Alerting.Secret = v
	}
	for _, e := range []struct {
		key string
		dst *int
	}{
		{"WB2A_ALERTING_INTERVAL_SECONDS", &c.Alerting.IntervalSeconds},
		{"WB2A_ALERTING_TIMEOUT_SECONDS", &c.Alerting.TimeoutSeconds},
		{"WB2A_ALERTING_STARTUP_GRACE_SECONDS", &c.Alerting.StartupGraceSeconds},
		{"WB2A_ALERTING_MIN_HEALTHY_CN", &c.Alerting.MinHealthyCN},
		{"WB2A_ALERTING_MIN_HEALTHY_GLOBAL", &c.Alerting.MinHealthyGlobal},
		{"WB2A_ALERTING_RECOVER_HEALTHY", &c.Alerting.RecoverHealthy},
		{"WB2A_ALERTING_BREAKER_THRESHOLD", &c.Alerting.BreakerThreshold},
		{"WB2A_ALERTING_FOR_TICKS", &c.Alerting.ForTicks},
		{"WB2A_ALERTING_CLEAR_TICKS", &c.Alerting.ClearTicks},
	} {
		if v := os.Getenv(e.key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*e.dst = n
			}
		}
	}
	if v := os.Getenv("WB2A_ALERTING_SEND_RESOLVE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Alerting.SendResolve = b
		}
	}
	if v := os.Getenv("WB2A_RETRY_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.RetryEnabled = b
		}
	}
	if v := os.Getenv("WB2A_RETRY_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.RetryAttempts = n
		}
	}
	if v := os.Getenv("WB2A_RETRY_BACKOFF"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Upstream.RetryBackoff = f
		}
	}
	if v := os.Getenv("WB2A_WEIGHT_DEFAULT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Pool.WeightDefault = f
		}
	}
	if v := os.Getenv("WB2A_MAX_CONCURRENCY_DEFAULT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pool.MaxConcurrencyDefault = n
		}
	}
	if v := os.Getenv("WB2A_MIN_INTERVAL_DEFAULT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Pool.MinIntervalDefault = f
		}
	}
	if v := os.Getenv("WB2A_MAX_TOTAL_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pool.MaxTotalConcurrency = n
		}
	}
	if v := os.Getenv("WB2A_ALERTING_MIN_CONSECUTIVE_FAILS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Alerting.MinConsecutiveFails = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	// 出口线路表：名字与地址都去空白；scheme 只认 http/https/socks5/socks5h。
	// 这里**启动即校验**：地址写错时让上游起不来（带明确提示），比跑起来以后每个
	// 请求都失败容易发现得多；线路名查不到（账号引用了不存在的名字）则在用时明确报错。
	if len(c.Proxies) > 0 {
		clean := make(map[string]string, len(c.Proxies))
		for name, addr := range c.Proxies {
			n := strings.TrimSpace(name)
			v := strings.TrimSpace(addr)
			if n == "" || v == "" {
				continue
			}
			// `{account}` 是会话模板占位符（面板支持），url.Parse 会因花括号报
			// "invalid userinfo"。校验时先换成固定示例会话名，真实地址在
			// TransportFor 里按账号渲染。
			probe := strings.ReplaceAll(v, "{account}", "probe")
			u, err := url.Parse(probe)
			if err != nil {
				return fmt.Errorf("proxies[%s]: %w", n, err)
			}
			switch strings.ToLower(u.Scheme) {
			case "http", "https", "socks5", "socks5h":
			default:
				return fmt.Errorf("proxies[%s]: 不支持的协议 %q（只能是 http/https/socks5/socks5h）", n, u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("proxies[%s]: 地址缺少主机名（%q）", n, v)
			}
			clean[n] = v
		}
		c.Proxies = clean
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	// 快过期窗口：空值回落默认 168h（Default 已置；此兜底覆盖显式 ""）；显式 "0"/负值 = 禁用分桶。
	if c.Pool.ExpiringSoon == "" {
		c.Pool.ExpiringSoon = "168h"
	}
	if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
		return fmt.Errorf("pool.expiring_soon: %w", err)
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0 // 负值视为禁用，避免 upstream 判定窗口反转
	}
	// 入站读取上限（吸收自 panel issue #100）：空值回落 300s；"0" = 不限制；
	// 负时长无意义，报错（处理风格同 pool.expiring_soon）。
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "300s"
	}
	if c.ServerReadTimeoutDur, err = time.ParseDuration(c.Server.ReadTimeout); err != nil {
		return fmt.Errorf("server.read_timeout: %w", err)
	}
	if c.ServerReadTimeoutDur < 0 {
		return fmt.Errorf("server.read_timeout: 负时长 %q 无意义（0 = 不限制）", c.Server.ReadTimeout)
	}
	if c.Server.MaxBodyMB < 0 {
		return fmt.Errorf("server.max_body_mb: %d 不得为负；0 = 不限制", c.Server.MaxBodyMB)
	}
	if c.Panel.SessionHours <= 0 {
		c.Panel.SessionHours = 24
	}
	if c.Pool.AccountDailyCreditLimit < 0 {
		c.Pool.AccountDailyCreditLimit = 0 // 负值视为关闭
	}
	// 国内版对话上游覆盖：空 = 内置默认；非空必须是 http(s) URL。
	if v := strings.TrimSpace(c.Upstream.CNChatBase); v != "" {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return fmt.Errorf("upstream.cn_chat_base: 必须以 http:// 或 https:// 开头")
		}
		c.Upstream.CNChatBase = v
	}
	// 11140 处置策略：空值回落 24h；"0" = 立即永久禁用（旧行为）；负时长报错。
	if strings.TrimSpace(c.Pool.AccountFaultHold) == "" {
		c.Pool.AccountFaultHold = "24h"
	}
	if c.AccountFaultHoldDur, err = time.ParseDuration(c.Pool.AccountFaultHold); err != nil {
		return fmt.Errorf("pool.account_fault_hold: %w", err)
	}
	if c.AccountFaultHoldDur < 0 {
		return fmt.Errorf("pool.account_fault_hold: 负时长 %q 无意义（0 = 立即禁用）", c.Pool.AccountFaultHold)
	}
	// 积分耗尽后的冷却时长（issue #78）：空 = 保持「硬冷却到次日 04:00」的旧行为；
	// 填了必须是正时长（负值会让冷却立即到期，等于没冷却 —— 与其静默放行，不如报错）。
	if v := strings.TrimSpace(c.Pool.CreditExhausted); v == "" {
		c.CreditExhaustedDur = 0
	} else {
		d, perr := time.ParseDuration(v)
		if perr != nil {
			return fmt.Errorf("cooldown.credit_exhausted: %w", perr)
		}
		if d <= 0 {
			return fmt.Errorf("cooldown.credit_exhausted: must be a positive duration, got %q", v)
		}
		c.CreditExhaustedDur = d
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	// （"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	// 上游重试段归一：Enabled=false 时惰性（字段存疑不影响启动）；Enabled=true
	// 时 Attempts 钳 [1,5]、Backoff 钳 [0.25,8]（越界按"填错单位"处理，钳到合法
	// 区间而非报错——与 mirror 的上界理由一致）。
	if c.Upstream.RetryEnabled {
		if c.Upstream.RetryAttempts < 1 {
			c.Upstream.RetryAttempts = 1
		}
		if c.Upstream.RetryAttempts > 5 {
			c.Upstream.RetryAttempts = 5
		}
		if c.Upstream.RetryBackoff < 0.25 {
			c.Upstream.RetryBackoff = 0.25
		}
		if c.Upstream.RetryBackoff > 8 {
			c.Upstream.RetryBackoff = 8
		}
	}
	// 账号级流量整形钳制：weight 钳 (0,1000]、限额钳 [0,256]/[0,3600]/[0,512]。
	if c.Pool.WeightDefault < 0 {
		c.Pool.WeightDefault = 1.0
	}
	if c.Pool.WeightDefault > 1000 {
		c.Pool.WeightDefault = 1000
	}
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.MaxConcurrencyDefault < 0 {
		c.Pool.MaxConcurrencyDefault = 0
	}
	if c.Pool.MaxConcurrencyDefault > 256 {
		c.Pool.MaxConcurrencyDefault = 256
	}
	if c.Pool.MinIntervalDefault < 0 {
		c.Pool.MinIntervalDefault = 0
	}
	if c.Pool.MinIntervalDefault > 3600 {
		c.Pool.MinIntervalDefault = 3600
	}
	if c.Pool.MaxTotalConcurrency < 0 {
		c.Pool.MaxTotalConcurrency = 0
	}
	if c.Pool.MaxTotalConcurrency > 512 {
		c.Pool.MaxTotalConcurrency = 512
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// fail-fast（设计 supplement §4.1）：admin.enabled=true 且 api_key 为空 = 未鉴权的
	// mutation 端点（disable/revive 是可用性操作，风险高于 /status 读泄漏），拒绝启动。
	// 校验放 applyEnv 之后：env 覆盖（WB2A_ADMIN_ENABLED / WB2A_API_KEY）与 config
	// 两条入口最终状态一致拦截。
	if c.Admin.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("admin.enabled=true 但 api_key 为空：请设置 api_key 或将 admin.enabled 置 false")
	}
	// 审计段的 fail-fast，与上面的 admin 校验并列（同属"开了功能就必须给齐参数"）：
	//   - audit_enabled=true 而 admin.enabled=false：审计的对象就是 /admin 端点，
	//     端点不存在时开着审计是个永远不会写一行的空配置——它比报错更危险，因为
	//     运维会以为"审计已经在跑了"。
	//   - 路径为空：无从落盘（Default 已给默认值，走到这里说明被显式置空了）。
	if c.Admin.AuditEnabled {
		if !c.Admin.Enabled {
			return fmt.Errorf("admin.audit_enabled=true 但 admin.enabled=false：审计的对象是 /admin 端点，请同时开启 admin.enabled")
		}
		if strings.TrimSpace(c.Admin.AuditFile) == "" {
			return fmt.Errorf("admin.audit_enabled=true 但 admin.audit_file 为空：请填写审计日志路径或将 admin.audit_enabled 置 false")
		}
	}
	// 预算闸：只有负值非法。0 是"不限"的合法哨兵，**不回落**——把 0 改写成某个
	// 默认上限，等于在老部署上凭空开始拒请求，正是"缺省 = 旧行为"要禁止的事。
	if c.Budget.DailyCreditLimit < 0 {
		return fmt.Errorf("budget.daily_credit_limit: %v 不得为负；0 = 关闭该闸（不限）", c.Budget.DailyCreditLimit)
	}
	// 告警段归一 + fail-fast（enabled=true 且 webhook_url 缺失/非法 → 拒绝启动）。
	// 位置与上面的 admin 校验并列：两者都是"开了功能就必须给齐参数"的启动期拦截。
	if err := c.normalizeAlerting(); err != nil {
		return err
	}
	// 排程段归一（空数组回落默认、ActivityReportCount 归一、小时范围校验）
	// 由 internal/config 统一实现，cmd/server 与 cmd/activity 共用同一份语义。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	// 分组密钥表归一并 fail-fast（空密钥 / 非法组名 / 密钥重复）。放在最后：它要用
	// 主 api_key 的**最终值**（env 覆盖之后）做去重比对。
	if err := c.normalizeAPIKeys(); err != nil {
		return err
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	return c.normalizePrompt()
}

// normalizeAlerting 归一并校验 alerting 段。
//
// 三件事：
//  1. 周期/宽限/拍数：0（未设置）回落默认；**负值报错**（负数无合理语义，静默回落
//     会掩盖配置笔误）。
//  2. 阈值：min_healthy_cn / min_healthy_global / breaker_threshold 的 **0 是合法
//     哨兵 = 关闭该规则**，不回落、不报错；只有负值报错。recover_healthy 的 0
//     回落默认 1（0 会让迟滞失效：healthy>=0 恒为真 → 告警立刻解除）。
//  3. enabled=true 时 webhook_url 必须非空且 scheme ∈ {http,https}、有主机名——
//     fail-fast 而非运行时空转（风格同 admin.enabled + 空 api_key）。
func (c *Config) normalizeAlerting() error {
	a := &c.Alerting

	for _, f := range []struct {
		name string
		val  int
	}{
		{"alerting.min_healthy_cn", a.MinHealthyCN},
		{"alerting.min_healthy_global", a.MinHealthyGlobal},
		{"alerting.breaker_threshold", a.BreakerThreshold},
		{"alerting.recover_healthy", a.RecoverHealthy},
		{"alerting.interval_seconds", a.IntervalSeconds},
		{"alerting.timeout_seconds", a.TimeoutSeconds},
		{"alerting.startup_grace_seconds", a.StartupGraceSeconds},
		{"alerting.for_ticks", a.ForTicks},
		{"alerting.clear_ticks", a.ClearTicks},
		{"alerting.min_consecutive_fails", a.MinConsecutiveFails},
	} {
		if f.val < 0 {
			return fmt.Errorf("%s: 不得为负", f.name)
		}
	}

	if a.IntervalSeconds == 0 {
		a.IntervalSeconds = 30
	}
	if a.TimeoutSeconds == 0 {
		a.TimeoutSeconds = 5
	}
	if a.StartupGraceSeconds == 0 {
		a.StartupGraceSeconds = 30
	}
	if a.ForTicks == 0 {
		a.ForTicks = 2
	}
	if a.ClearTicks == 0 {
		a.ClearTicks = 2
	}
	if a.RecoverHealthy == 0 {
		a.RecoverHealthy = 1
	}

	if a.Enabled {
		raw := strings.TrimSpace(a.WebhookURL)
		if raw == "" {
			return fmt.Errorf("alerting.enabled=true 但 alerting.webhook_url 为空：请填写告警投递地址或将 alerting.enabled 置 false")
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("alerting.webhook_url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("alerting.webhook_url: scheme 必须是 http 或 https，got %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("alerting.webhook_url: 缺少主机名")
		}
	}
	return nil
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 custom/append/passthrough）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
// apiKeyGroupRe 分组名的合法字符集：小写字母/数字开头，其后可含小写字母、数字、下划线、
// 连字符，总长 ≤32。
//
// 为什么要卡大小写与空白：组名要靠"与账号 auth 文件里的 groups **精确相等**"才生效
// （auth.MatchesGroups 不做大小写折叠）。配置侧宽松的话，一个 "Internal" 会安静地
// 匹配不到任何标了 "internal" 的账号 —— 表现为"配了分组却一把号都拿不到"，而配置
// 本身看起来完全正常。故在此 fail-fast，把错误挡在启动期。
var apiKeyGroupRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// normalizeAPIKeys 归一并校验 api_keys 段（分组密钥表）。
//
// fail-fast 三则（风格同 admin.enabled + 空 api_key：开了功能就必须给齐参数）：
//  1. key 为空 → 报错。空密钥等于"任何空 Bearer 都能过"，是纯提权而非笔误容错。
//  2. 组名不合规（见 apiKeyGroupRe）→ 报错（理由见该变量注释）。
//  3. 密钥指纹重复（**含与主 api_key 重复**）→ 报错。匹配是遍历后取最后一个命中，
//     重复条目的 groups 绑定会被静默忽略 —— 配了不生效，比配错更难发现。
//
// groups 归一：去首尾空白、丢弃空串、按首次出现顺序去重；整段为空切片 = 不限分组
// （合法语义，不是错误）。空数组与"省略 groups 键"等价。
func (c *Config) normalizeAPIKeys() error {
	if len(c.APIKeys) == 0 {
		return nil
	}
	fpSeen := make(map[string]string, len(c.APIKeys)+1) // 指纹 → 人类可读的条目描述
	if k := strings.TrimSpace(c.APIKey); k != "" {
		fpSeen[apiKeyFP(k)] = "api_key（主密钥）"
	}
	for i := range c.APIKeys {
		e := &c.APIKeys[i]
		e.Key = strings.TrimSpace(e.Key)
		if e.Key == "" {
			return fmt.Errorf("api_keys[%d].key 为空：空密钥等于放行任何空 Bearer，请填写密钥或删除该条目", i)
		}
		e.Name = strings.TrimSpace(e.Name)
		label := fmt.Sprintf("api_keys[%d]", i)
		if e.Name != "" {
			label += "（" + e.Name + "）"
		}
		fp := apiKeyFP(e.Key)
		if prev, dup := fpSeen[fp]; dup {
			return fmt.Errorf("%s 与 %s 的密钥相同（指纹 %s）：重复密钥会让后者的 groups 绑定被静默忽略",
				label, prev, fp)
		}
		fpSeen[fp] = label

		if len(e.Groups) == 0 {
			e.Groups = nil
			continue
		}
		norm := make([]string, 0, len(e.Groups))
		gSeen := make(map[string]bool, len(e.Groups))
		for _, g := range e.Groups {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			if !apiKeyGroupRe.MatchString(g) {
				return fmt.Errorf("%s.groups 含非法组名 %q：须匹配 %s（组名要与账号文件的 groups 精确相等才生效，不接受大写与空白）",
					label, g, apiKeyGroupRe.String())
			}
			if gSeen[g] {
				continue
			}
			gSeen[g] = true
			norm = append(norm, g)
		}
		if len(norm) == 0 {
			norm = nil
		}
		e.Groups = norm
	}
	return nil
}

// apiKeyFP 密钥指纹（sha256 前 16 位 hex）。与 nginx 侧的 sync-nginx-key.sh、
// internal/server 的审计指纹同口径，仅用于去重与日志溯源，**不落明文**。
func apiKeyFP(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])[:16]
}

// buildAuthKeys 把配置展开成 handler 的鉴权密钥表。
//
// 主密钥（cfg.APIKey）排第一且 Groups 为空 = 不限分组，保持"主密钥 = 全权"的既有
// 语义：既有客户端拿的是同一把 key，行为与改动前逐字一致。
// 分组密钥随后追加，各自携带自己的可见分组。
//
// 两者都为空 → 返回 nil，handler 视作"不鉴权部署"（现状语义）。
func buildAuthKeys(cfg *Config) []server.AuthKey {
	// 刻意用 nil 零值切片而非 make(...,0,...)：全空时返回 nil（"无非配置"的干净语义），
	// 与 handler 侧 len()>0 的判断等价，但避免出现空切片与非 nil 的差异。
	var out []server.AuthKey
	if k := strings.TrimSpace(cfg.APIKey); k != "" {
		out = append(out, server.AuthKey{Key: k, Name: "主密钥"})
	}
	for _, e := range cfg.APIKeys {
		out = append(out, server.AuthKey{Key: e.Key, Name: e.Name, Groups: e.Groups, Models: e.Models, Realm: e.Realm})
	}
	return out
}

func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / append / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// ParseConfig 从 JSON 字节解析配置（Default + Unmarshal + normalize），供面板保存配置时校验：
// 不读环境变量（避免 env 覆盖面板提交值），不写盘。
func ParseConfig(raw []byte) (*Config, error) {
	c := Default()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}
