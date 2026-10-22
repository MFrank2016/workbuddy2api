// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// ProxyName 账号级出口线路**名**（不是地址），来源 auth 文件顶层 `proxy` 字段，
	// 由面板的「出口线路」功能写入；线路名 → 代理地址的映射在上游 config.json 的
	// `proxies` 里。空 = 跟随进程级代理（WB_HTTP_PROXY / 环境变量）。
	//
	// 为什么存在这个字段：面板早就能给账号绑线路，但上游此前完全不读它 —— 于是
	// 「每个号一条出口」只是界面上看起来生效，真正的对话流量还是全池共用宿主出口
	// （多号同 IP 容易被平台关联）。现在上游按它选 transport（见 upstream.TransportFor）。
	ProxyName string

	// ProxySession 旧版 `{account}` 线路模板用的会话名（auth 文件顶层 proxy_session）。
	// 面板存下来是为了「之后改昵称不换出口 IP」；为空时按昵称/uid 现算（见 SessionName）。
	ProxySession string

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// Groups 账号的**业务分组标签**（如 ["internal","external"]），落盘于文件**顶层**
	// groups 键（嵌套形/扁平形同一位置，与 device_token 同风格）。
	//
	// 与 realm 的分工（两者正交，选号时 AND 组合）：
	//   realm —— 账号**固有**的技术域，由 domain 决定、网关强制归一化为 cn/global，
	//            **不可自定义**（见 realmLocked）；
	//   Groups —— **运维自定义**的业务标签，可多个，用来把"哪把密钥能用它"这件事
	//            从技术域里解耦出来。
	//
	// 语义：密钥带 groups 时，只有与之**有交集**的账号参与选号；密钥不带 groups
	// （主密钥 / 未配置）时不过滤。账号 Groups 为空 = **未分组**，默认**不被任何
	// 分组密钥选中**（默认拒绝：宁可漏用，不可串组）。
	//
	// 并发：仅 Parse/LoadDir 赋值一次，其后整个生命周期只读（热加载换的是新的
	// *Auth 指针，而不是改这个字段），故不纳入 a.mu 保护 —— 与 realm 不同
	// （realm 会被 RefreshToken 在锁内改写）。
	Groups []string
	// Weight 账号级选号权重显式值（乘子）。**指针语义**：nil = 未配置，回落
	// pool.weight_default（默认 1.0 = 等价纯三因子权重，行为逐字一致）；
	// 显式 0 = 只读备用（不参与自动轮转）。json.Unmarshal 缺键 → nil，
	// 显式 0 → 非 nil 指针——这是「未配置」与「显式停用」的唯一可靠区分。
	// 与 Groups 同生命周期：仅 Parse/LoadDir 赋值一次，其后只读，不纳入 a.mu。
	Weight *float64 `json:"weight"`
	// MaxConcurrency 账号级并发上限显式值。0/缺键 = 未配置，回落
	// pool.max_concurrency_default（默认 0 = 不限）。0 本身就是「不限」，
	// 无歧义，用 flat 零值。生命周期同 Weight。
	MaxConcurrency int `json:"max_concurrency"`
	// MinInterval 账号级最小请求间隔显式值（秒）。0/缺键 = 未配置，回落
	// pool.min_interval_default（默认 0 = 不限）。软约束：全部被限住时回落
	// LRU 兜底，不报「暂无可用账号」。生命周期同 Weight。
	MinInterval float64 `json:"min_interval"`
	// DisabledModels 账号级禁用模型（auth 文件顶层 disabled_models，支持后缀 "*" 通配）。
	// 选号时该账号对命中模型不可用（上游 11158/11102 的本地前置规避）。
	// 生命周期同 Groups/Weight：Parse 后只读。
	DisabledModels []string

	// Profile 账号的产品 profile（cn-cli / cn-work / intl-cli / intl-work），
	// 与 realm **正交**：realm 决定「国内/国际」，profile 决定「CLI/工作台」。
	//
	// 来源与优先级（契约 §1，唯一选择链）：
	//   explicit（顶层 profile）> persisted derived（顶层 profile_derived，
	//   仅 profile_backfill_v1=true 时可信）> stored realm（cn→cn-cli、
	//   global→intl-work）> domain（4-host 表）> cn-cli。
	//
	// Parse 时算好，之后只读；**Parse 不回写**。显式值与推导值分别由
	// profileExplicit / ProfileDerived 持有，SaveAtomic 按原样落盘。
	Profile string
	// ProfileDerived 已持久化的**推导** profile（顶层 profile_derived）。
	// 只由 BackfillRealm 在落盘 realm 时写入 —— 用来记住「这个 domain 推导出
	// 什么 profile」，使无 realm 的旧账号在重启后仍能按原 profile 解析。
	ProfileDerived string

	// profileExplicit 顶层 profile 的原始显式值（空 = 未显式指定）。
	// 与 Profile 分开持有：Profile 是计算结果，写回时只能写用户真正设过的值，
	// 否则一次 token 刷新就会把推导结果固化成显式选择（见 SaveAtomic）。
	profileExplicit string
	// profileBackfillV1 顶层 profile_backfill_v1：标记 profile_derived 是本进程
	// 落盘的推导值（可信）。缺失/false 时 profile_derived 不参与选择。
	profileBackfillV1 bool
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争，go test -race 实证：
//
//	WARNING: DATA RACE
//	Write at ... by goroutine:
//	  (*Client).RefreshToken()  internal/upstream/client.go:929
//	Previous read at ... by goroutine:
//	  (*Client).ChatHeaders()   internal/upstream/headers.go:224
//
// （回归测试 upstream.TestChatHeadersRacesRefreshToken）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
//
// 与 #125 修的 AccessToken/Domain 属同一类：守卫是纯读、刷新是纯写，二者无同步即
// 构成数据竞争（RefreshToken 写回在 client.go「第 2 段（锁内）」的 `if tok.RefreshToken
// != ""` 分支）。go test -race 实证（回归测试 scheduler.TestKeepaliveGuardRacesRefreshToken）：
//
//	WARNING: DATA RACE
//	Read at ... by goroutine:
//	  (*Scheduler).RunKeepaliveNow()  internal/scheduler/scheduler.go:654
//	Previous write at ... by goroutine:
//	  (*Client).RefreshToken()        internal/upstream/client.go:931
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// ExpiresAtValue 加锁读取 access token 的到期时刻（Unix 秒）。RefreshToken 在锁内
// 改写它，故跨包只读也必须经此取值（同 AccessTokenValue/DomainValue/RefreshTokenValue）。
//
// 用途：把「令牌到期」从**控制台读文件的旁路**（console/server.py 直接读 auths/*.json
// 的 expiresAt）搬回网关权威侧 —— 文件值是「上次写回时的快照」，而池内进程持有的
// ExpiresAt 才是选号/续期判定真正用的那个值。两者不一致（如文件陈旧但进程内已续期）
// 时，运维看到的就是一个不存在的「已过期」。
func (a *Auth) ExpiresAtValue() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ExpiresAt
}

// CredWrittenAt 返回凭证文件（FilePath）的最近写回时刻，ok=false 表示文件不可 stat
// （不存在/无权限/FilePath 为空）。
//
// 为什么要它：区分两种「过期显示」——(1) 续期链路正常，文件刚被 SaveAtomic 写过，
// 只是 access token 本身寿命短；(2) 文件长期未更新，说明续期根本没跑（账号被
// disabled 后 RunKeepaliveNow 会 continue 跳过它，见 scheduler.go）——此时「过期」
// 是病因的**症状**，不是原因。只看 expiresAt 一个数字区分不出这两者。
//
// 不返回 error：巡检/状态透出是**尽力而为**的观测，缺文件不该让它失败或报错。
func (a *Auth) CredWrittenAt() (time.Time, bool) {
	if a == nil || a.FilePath == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(a.FilePath)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// ResolveProfile 按契约 §1 的**唯一选择链**推导产品 profile：
//
//	explicit > persisted derived（仅当 backfillV1）> rawRealm > domain > cn-cli
//
// 三条容易写错的约束（均有测试钉住）：
//  1. rawRealm 必须是 **BackfillRealm 之前**的原始 realm 字符串。BackfillRealm
//     会给旧文件补一个 realm（www.codebuddy.ai / www.workbuddy.cn 补的是 cn），
//     若拿补过的值参与推导，这两个 host 的 profile 就退化了。
//  2. derived 只在 backfillV1 为 true 时可信：否则面板/用户手写的
//     profile_derived 会意外压过 realm，与「显式才生效」的直觉相反。
//  3. realm 只认 cn / global 两个字面值；其它值（含空串）视为「没有 realm」，
//     继续看 domain —— 空串绝不能当 cn，否则 domain 分支永远走不到。
func ResolveProfile(explicit, derived, rawRealm, domain string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	if p := strings.TrimSpace(derived); p != "" {
		return p
	}
	switch strings.TrimSpace(rawRealm) {
	case "global":
		return "intl-work"
	case "cn":
		return "cn-cli"
	}
	if p := profileForDomain(domain); p != "" {
		return p
	}
	return "cn-cli"
}

// profileForDomain 4-host 表（契约 §1「典型 domain」列）。
//
// 只列能**唯一**确定 profile 的 host：codebuddy.cn / workbuddy.ai 被多个
// profile 共用，猜错会把请求打到另一个产品的端点上（比不猜更糟）。
func profileForDomain(domain string) string {
	switch normalizeDomain(domain) {
	case "copilot.tencent.com":
		return "cn-cli"
	case "www.workbuddy.cn":
		return "cn-work"
	case "www.codebuddy.ai":
		return "intl-cli"
	case "www.workbuddy.ai":
		return "intl-work"
	}
	return ""
}

// firstNonEmpty 返回第一个去空白后非空的值（顶层键优先于兼容位置）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// normalizeDomain 把 auth 文件里的 domain 归一成裸 host。
//
// auth 文件里 domain 通常就是 `copilot.tencent.com`，但手写/导入的形态可能带
// scheme、端口、路径（`https://WWW.WorkBuddy.CN/`）。不归一化的话 4-host 表
// 只能匹配其中一种写法，另一种静默落回 cn-cli —— 那是「配置看着对、行为不对」
// 的典型形态。
func normalizeDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return ""
	}
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexByte(d, '/'); i >= 0 {
		d = d[:i]
	}
	if i := strings.IndexByte(d, '@'); i >= 0 {
		d = d[i+1:]
	}
	// 端口：只在最后一段是数字时剥掉（IPv6 字面量不在 4-host 表里，不处理）。
	if i := strings.LastIndexByte(d, ':'); i >= 0 {
		if _, err := strconv.Atoi(d[i+1:]); err == nil {
			d = d[:i]
		}
	}
	return strings.TrimSuffix(d, ".")
}

// ProfileRealm 由 profile 推导 realm（契约 §1：cn-* → cn、intl-* → global）。
//
// 只认两个前缀；认不出返回空串（**不编造** realm —— 猜错会让 core 按错误的
// 版本选号）。供 search/MCP 入口与 manager 头推导共用同一份判据。
func ProfileRealm(profile string) string {
	p := strings.ToLower(strings.TrimSpace(profile))
	switch {
	case strings.HasPrefix(p, "intl-"):
		return "global"
	case strings.HasPrefix(p, "cn-"):
		return "cn"
	}
	return ""
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	// 用 BackfillRealm **之前**的原始 realm（这里是空串）与 domain 推导 profile，
	// 与 Parse 里同一份判据 —— 两处各写一遍迟早漂移。
	// 推导值**不含显式覆盖**：它记的是「这个 domain 推导出什么」，显式选择另存
	// profileExplicit，两者互不覆盖（否则显式值会把推导证据抹掉）。
	a.ProfileDerived = ResolveProfile("", "", "", a.Domain)
	a.Profile = ResolveProfile(a.profileExplicit, a.ProfileDerived, "", a.Domain)
	a.profileBackfillV1 = true
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（enterpriseId 非空）。
// 企业版无个人成长体系（签到/成长任务/连登/旅行/夜猫子 上游均拒），
// 调度与面板据此跳过对应操作；选号、保活、额度展示不受影响。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// BackfillRealmFor 把账号的 realm 显式置为 cn/global（面板 OAuth 登录后补标识用）。
// 返回是否发生变化；realm 非法返回错误。调用方负责随后 SaveAtomic 落盘。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// GroupsValue 返回账号分组标签的**副本**（调用方可能缓存或跨 goroutine 传递，
// 返回副本以免误改内部切片）。
func (a *Auth) GroupsValue() []string {
	if a == nil || len(a.Groups) == 0 {
		return nil
	}
	out := make([]string, len(a.Groups))
	copy(out, a.Groups)
	return out
}

// WeightValue 返回账号权重显式值与其「已配置」标志：configured=false 时
// 调用方（pool）应回落池级默认。weight 只在 Parse 赋值一次、其后只读，
// 与 GroupsValue 同生命周期（不纳入 a.mu），读指针值无需加锁。
func (a *Auth) WeightValue() (float64, bool) {
	if a == nil || a.Weight == nil {
		return 0, false
	}
	return *a.Weight, true
}

// MaxConcurrencyValue 返回账号并发上限显式值（0 = 未配置，回落池级默认）。
func (a *Auth) MaxConcurrencyValue() int {
	if a == nil {
		return 0
	}
	return a.MaxConcurrency
}

// MinIntervalValue 返回账号最小间隔显式值（秒，0 = 未配置，回落池级默认）。
func (a *Auth) MinIntervalValue() float64 {
	if a == nil {
		return 0
	}
	return a.MinInterval
}

// MatchesGroups 报告本账号对「允许分组」是否可见：
//
//	want 为空    → 恒 true（不限分组：主密钥 / 未配置 groups 的密钥语义）

// MatchesGroups 报告本账号对「允许分组」是否可见：
//
//	want 为空    → 恒 true（不限分组：主密钥 / 未配置 groups 的密钥语义）
//	账号未打标签 → 恒 false（**默认拒绝**：未分组的账号不给任何分组密钥用）
//	否则         → 要求交集非空
//
// 比较是**精确相等**（不做大小写折叠、不做前缀匹配）：组名规范由密钥侧配置
// （cmd/server/config.go 的正则 fail-fast）与控制台把关，读侧保持"读进来是什么
// 就是什么"，避免两侧规则不一致导致的静默错配。
func (a *Auth) MatchesGroups(want []string) bool {
	if len(want) == 0 {
		return true
	}
	if a == nil || len(a.Groups) == 0 {
		return false
	}
	for _, w := range want {
		for _, g := range a.Groups {
			if w == g {
				return true
			}
		}
	}
	return false
}

// normalizeGroups 归一化账号分组标签：去首尾空白、丢弃空串、按首次出现顺序去重。
//
// 刻意**不做字符集校验**：组名规范由密钥侧配置与控制台把关，读侧保持宽松——否则
// 手写文件里一个尾随空格就会让账号在热加载后静默消失，而热加载是 5 秒一次的常态
// 路径（internal/pool/watch.go），这类静默失配的排查成本极高。
func normalizeGroups(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	// 客户端 at-rest 加密信封（WorkBuddy 5.6+）必须在反序列化**之前**识别：
	// 信封是对象而非字符串，先让它撞进 json.Unmarshal 会得到
	// 「cannot unmarshal object into ... type string」的 storage_parse_error，
	// 把用户引向「文件坏了」的错误方向。见 envelope.go（不实现解密）。
	if err := rejectEncryptedEnvelope(raw); err != nil {
		return nil, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
				// Profile 兼容读：契约规定 profile 在**顶层**，但 realm 就在 auth
				// 段里，手写时把 profile 一起塞进 auth 是很自然的写法。不认它，
				// 用户会看到「配置明明写了 intl-cli、实际却按 domain 推导」——
				// 正是本仓库反复出现的「看着对、行为不对」形态。顶层优先。
				Profile string `json:"profile"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用）。
			// 放在 auth 段之外，手写时无需嵌进 auth 对象，降低配置门槛。
			DeviceToken string `json:"device_token"`
			// Proxy 顶层 proxy（线路名，面板写入；见 Auth.ProxyName）。
			Proxy string `json:"proxy"`
			// ProxySession 顶层 proxy_session：旧版 `{account}` 模板用的会话名。
			// 面板存下来就为了「之后改昵称不换出口 IP」，所以优先用它渲染。
			ProxySession string `json:"proxy_session"`
			// Groups 顶层 groups（嵌套形与扁平形共用）：账号业务分组标签。
			Groups []string `json:"groups"`
			// 账号级流量整形（v0.1.3 移植）：顶层 weight/max_concurrency/min_interval。
			// weight 用 *float64：缺键 nil = 回落池级默认，显式 0 = 只读备用。
			Weight         *float64 `json:"weight"`
			MaxConcurrency int      `json:"max_concurrency"`
			MinInterval    float64  `json:"min_interval"`
			DisabledModels []string `json:"disabled_models"`
			// 产品 profile（顶层，与 device_token/groups 同位置）。三段：
			// profile 是显式选择，profile_derived 是本进程落盘的推导值，
			// profile_backfill_v1 标记后者可信（见 ResolveProfile 的优先级）。
			Profile           string `json:"profile"`
			ProfileDerived    string `json:"profile_derived"`
			ProfileBackfillV1 bool   `json:"profile_backfill_v1"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:    n.Auth.AccessToken,
			RefreshToken:   n.Auth.RefreshToken,
			ExpiresAt:      n.Auth.ExpiresAt,
			Domain:         n.Auth.Domain,
			realm:          n.Auth.Realm,
			UID:            n.Account.UID,
			EnterpriseID:   n.Account.EnterpriseID,
			Nickname:       n.Account.Nickname,
			DeviceToken:    n.DeviceToken,
			ProxyName:      strings.TrimSpace(n.Proxy),
			ProxySession:   strings.TrimSpace(n.ProxySession),
			Groups:         normalizeGroups(n.Groups),
			Weight:         n.Weight,
			MaxConcurrency: n.MaxConcurrency,
			MinInterval:    n.MinInterval,
			DisabledModels: normalizeModels(n.DisabledModels),

			ProfileDerived:    strings.TrimSpace(n.ProfileDerived),
			profileExplicit:   firstNonEmpty(n.Profile, n.Auth.Profile),
			profileBackfillV1: n.ProfileBackfillV1,
		}
	} else {
		var f struct {
			AccessToken       string   `json:"accessToken"`
			RefreshToken      string   `json:"refreshToken"`
			ExpiresAt         int64    `json:"expiresAt"`
			Domain            string   `json:"domain"`
			Realm             string   `json:"realm"`
			UID               string   `json:"uid"`
			EnterpriseID      string   `json:"enterpriseId"`
			Nickname          string   `json:"nickname"`
			DeviceToken       string   `json:"device_token"`
			Proxy             string   `json:"proxy"`
			ProxySession      string   `json:"proxy_session"`
			Groups            []string `json:"groups"`
			Weight            *float64 `json:"weight"`
			MaxConcurrency    int      `json:"max_concurrency"`
			MinInterval       float64  `json:"min_interval"`
			DisabledModels    []string `json:"disabled_models"`
			Profile           string   `json:"profile"`
			ProfileDerived    string   `json:"profile_derived"`
			ProfileBackfillV1 bool     `json:"profile_backfill_v1"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:    f.AccessToken,
			RefreshToken:   f.RefreshToken,
			ExpiresAt:      f.ExpiresAt,
			Domain:         f.Domain,
			realm:          f.Realm,
			UID:            f.UID,
			EnterpriseID:   f.EnterpriseID,
			Nickname:       f.Nickname,
			DeviceToken:    f.DeviceToken,
			ProxyName:      strings.TrimSpace(f.Proxy),
			ProxySession:   strings.TrimSpace(f.ProxySession),
			Groups:         normalizeGroups(f.Groups),
			Weight:         f.Weight,
			MaxConcurrency: f.MaxConcurrency,
			MinInterval:    f.MinInterval,
			DisabledModels: normalizeModels(f.DisabledModels),

			ProfileDerived:    strings.TrimSpace(f.ProfileDerived),
			profileExplicit:   strings.TrimSpace(f.Profile),
			profileBackfillV1: f.ProfileBackfillV1,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	// profile 必须在 **BackfillRealm 之前**算：Parse 本身不 backfill，但 LoadDir
	// 随后会做。这里用的 a.realm / a.Domain 都还是磁盘原值（未经任何改写）。
	//
	// 关键：**不回写 profile**。a.Profile 只是内存里的计算结果，落盘由 SaveAtomic
	// 按 profileExplicit / ProfileDerived 分别处理 —— 把推导值写成显式 profile 会让
	// 一次 token 刷新永久固化语义（见 SaveAtomic 的注释与 profile_test.go）。
	derived := ""
	if a.profileBackfillV1 {
		derived = a.ProfileDerived
	}
	a.Profile = ResolveProfile(a.profileExplicit, derived, a.realm, a.Domain)
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	// 先读回磁盘上的现有内容：保留本函数不认识的键，并**绝不覆盖** proxy /
	// proxy_session（见下方说明）。
	doc := map[string]any{}
	if raw, err := os.ReadFile(a.FilePath); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
			doc = map[string]any{}
		}
	}
	doc["auth"] = map[string]any{
		"accessToken":  a.AccessToken,
		"refreshToken": a.RefreshToken,
		"expiresAt":    a.ExpiresAt,
		"domain":       a.Domain,
		"realm":        a.realm,
	}
	doc["account"] = map[string]any{
		"uid":          a.UID,
		"enterpriseId": a.EnterpriseID,
		"nickname":     a.Nickname,
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// ★ proxy / proxy_session 不在这里写回 —— 它们的所有权归磁盘（面板）★
	//
	// 这两个字段的唯一写入方是 workbuddy-manager（write_auth_file / set_account_proxy）。
	// 本进程内的 *Auth 最多滞后一个热加载周期（5 秒），于是：
	//   · 用内存值「非空才写」→ 面板刚绑定、尚未热加载的那次冷却/刷新写回会用旧空值
	//     把绑定抹掉（实测发生过：一个高频冷却的账号绑定自己消失了）；
	//   · 用「内存值总是写」→ 面板**解绑**后，内存里仍是旧线路名，写回会把解绑复活。
	// 两种都错。正确做法是：这两个键一律保持磁盘原样（上面的 doc 已从磁盘读入）。
	// Groups 非空才写回顶层 groups（与 device_token 同策略：不给无该字段的旧文件引入空键）。
	//
	// ★ 必须写回 ★ 本函数用**固定字段集整体重写**文件，漏掉哪个键就等于把那个键从磁盘上
	// 删掉。账号的 token 刷新（RefreshToken → SaveAtomic）是高频路径，漏写 groups 会让
	// 分组标签"自己消失"——而且是静默的：标签没了 → 默认拒绝策略下账号被所有分组密钥
	// 忽略 → 表现为"某分组突然无号可用"，但文件里看不到任何报错。
	if len(a.Groups) > 0 {
		doc["groups"] = a.Groups
	}
	// DisabledModels 非空才写回（同 groups 策略）。★ 必须写回 ★ 否则 token 刷新会
	// 静默清掉账号级禁用模型。
	if len(a.DisabledModels) > 0 {
		doc["disabled_models"] = a.DisabledModels
	}
	// profile 三键（顶层）：与 device_token/groups 同策略 —— 只在有值/已置位时写，
	// 不给旧文件引入空键。
	//
	// ★ 只写**显式值**，绝不写推导结果 ★
	// 本函数用固定字段集整体重写文件，所以漏写 = 每次 token 刷新都把管理员设的
	// profile 静默删掉；但反过来「无条件写 a.Profile」同样有害：推导值一旦被写成
	// 显式 profile，之后管理员把 realm 改成 global 也不再得到 intl-work（显式优先），
	// 语义被一次刷新永久固化。推导证据走 profile_derived + 版本标记这条路。
	if a.profileExplicit != "" {
		doc["profile"] = a.profileExplicit
	}
	if a.ProfileDerived != "" {
		doc["profile_derived"] = a.ProfileDerived
	}
	if a.profileBackfillV1 {
		doc["profile_backfill_v1"] = true
	}
	// 账号级流量整形（v0.1.3）：与 device_token/groups 同策略——只在显式配置时
	// 写回，不给旧文件引入空键。★ 必须写回 ★ 本函数固定字段集整体重写，漏掉
	// weight/max_concurrency/min_interval 就等于在每次 token 刷新时把用户配的
	// 整形旋钮静默删掉（weight=0 的只读备用号会「自己复活」参与轮转）。
	// weight 用指针：非 nil 才写（保留「显式 0 = 只读备用」语义，区别于未配置）。
	if a.Weight != nil {
		doc["weight"] = *a.Weight
	}
	if a.MaxConcurrency != 0 {
		doc["max_concurrency"] = a.MaxConcurrency
	}
	if a.MinInterval != 0 {
		doc["min_interval"] = a.MinInterval
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// 权限与属主：跨 uid 场景的核心（详见 atomicwrite.go）——已有文件沿用磁盘上的
	// mode 与 uid/gid，新文件用 FileMode()（默认 0600）；chown 失败只 warning。
	return writeAtomicWithMode(a.FilePath, raw, logfmt.Label(a.UID, a.Nickname))
}

// AuthFileGlob auth 文件的统一 glob 模式（宽侧：workbuddy*.json）。
// 网关 LoadDir 与 cmd 运维工具（signin/credit/trial）共用此单一来源——
// 此前 cmd 侧私用 workbuddy-*.json 窄模式，不带连字符的文件（如
// workbuddy_new.json）被网关加载却被运维工具跳过，排障口径对不上
// （审查发现 10）。
const AuthFileGlob = "workbuddy*.json"

// LoadAuthFiles 返回 dir 下按 AuthFileGlob 匹配的 auth 文件清单（已排序）。
// 供 cmd 运维工具复用：只列文件、不解析不迁移（LoadDir 才做 backfill 等副作用），
// 保持 signin/credit/trial 原有的「逐文件 Parse、损坏即跳过/报行错」流程不变。
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// ModelDisabled 报告账号是否禁用指定模型（精确或后缀 "*" 通配）。
func (a *Auth) ModelDisabled(model string) bool {
	if a == nil || model == "" || len(a.DisabledModels) == 0 {
		return false
	}
	for _, p := range a.DisabledModels {
		if p == model {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// normalizeModels 去空白/空串（与 normalizeGroups 同风格）。
func normalizeModels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SessionName 这个账号在 `{account}` 线路模板里使用的会话名。
//
// 与面板 `config.route_session_name` 同一套规则（两边各有一份是因为跑在不同进程里）：
//  1. 优先用存下来的 proxy_session（改昵称不会悄悄换掉出口 IP）；
//  2. 否则按昵称生成 slug：只留 ASCII 字母数字、其余换 '-'、折叠连续 '-'、截断 24；
//  3. slug 不足 3 个字符时退回 uid 前 8 位，再不行用 "acct"。
//
// 为什么必须有它：线路地址里的 `{account}` 是**模板**，`url.Parse` 会直接拒绝花括号
// （invalid userinfo）。上游必须自己把它渲染成具体会话名再交给 http.Transport。
func (a *Auth) SessionName() string {
	if a == nil {
		return "acct"
	}
	if s := strings.TrimSpace(a.ProxySession); s != "" {
		return s
	}
	var b strings.Builder
	for _, r := range strings.ToLower(a.Nickname) {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	slug := b.String()
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if len(slug) > 24 {
		slug = slug[:24]
	}
	if len(slug) >= 3 {
		return slug
	}
	if uid := strings.TrimSpace(a.UID); uid != "" {
		if len(uid) > 8 {
			return uid[:8]
		}
		return uid
	}
	return "acct"
}
