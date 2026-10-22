// productprofile.go 产品 profile 的端点表与两个**独立**的 host 选择轴（契约 §1）。
//
// 与 profile.go 无关：那个是「账号资料」（昵称同步），本文件是「产品 profile」
// （cn-cli / cn-work / intl-cli / intl-work）的端点路由。
//
// 两个轴必须分开，不能压成一条链：
//
//	① profile 选择：explicit > persisted derived（仅 profile_backfill_v1=true）
//	   > stored realm > domain > cn-cli。由 auth.Parse 在装载时算好（a.Profile）。
//	② host 选择：profile 选定之后，**只有 chat host 可被配置覆盖**：
//	   · upstream.cn_chat_base → 仅 cn-* 的 chat host
//	   · global.chat_base      → 仅 intl-* 的 chat host
//	   · global.billing_base   → 仅 intl-* 的 billing host
//	   cn-* 的 billing base 与**所有** profile 的 auth base 一律取表值（无覆盖键）。
//	   webBase 恒取 web base 列，不受任何覆盖影响。
//
// 路由只在 profiles.enabled=true 且 profiles.detect_only=false 时生效；
// 其余组合（含默认全关）逐字走旧的 chatBase(a)/globalOn(a) 路径。
package upstream

import (
	"errors"
	"strings"

	"workbuddy2api/internal/auth"
)

// ProductCaps 各 profile 的「上游业务能力」矩阵（取值以仓库现有门控为准）。
//
// 现有事实：国际版账号没有 CN 的签到/成长任务体系——
// scheduler.go 对 global 跳过签到与 growth（/activity/growth/streak 实测 500）、
// travel.go / blackcat.go / panel/taskcenter.go 同样按 IsGlobal 跳过，
// School 是 CN 专属活动脚本；billing（余额/报表）两域都有。
// 因此 cn-* 六项全 true，intl-* 只有 Billing。
//
// Caps **只在** profiles 激活（enabled=true 且 detect_only=false）时可用，
// 且不参与选号（Todo 22）。
type ProductCaps struct {
	Checkin     bool
	Travel      bool
	School      bool
	BlackCat    bool
	GrowthDaily bool
	Billing     bool
}

// ProductProfile 一个产品 profile 的端点描述符。
type ProductProfile struct {
	ID          string
	ChatBase    string
	BillingBase string
	WebBase     string
	AuthBase    string
	Origin      string
	UAPlatform  string
	Domain      string
	Caps        ProductCaps
}

// cnAllCaps CN 账号具备全部六项业务能力。
var cnAllCaps = ProductCaps{
	Checkin: true, Travel: true, School: true, BlackCat: true, GrowthDaily: true, Billing: true,
}

// intlBillingOnly 国际版只有 billing（余额/报表）。
var intlBillingOnly = ProductCaps{Billing: true}

// productProfileTable 契约 §1 的 4-profile 端点表（逐列对应文档表格）。
var productProfileTable = map[string]ProductProfile{
	"cn-cli": {
		ID:          "cn-cli",
		ChatBase:    "https://copilot.tencent.com",
		BillingBase: "https://www.codebuddy.cn",
		WebBase:     "https://www.workbuddy.cn",
		AuthBase:    "https://copilot.tencent.com",
		Origin:      "https://www.codebuddy.cn",
		UAPlatform:  "WorkBuddy",
		Domain:      "copilot.tencent.com",
		Caps:        cnAllCaps,
	},
	"cn-work": {
		ID:          "cn-work",
		ChatBase:    "https://www.workbuddy.cn",
		BillingBase: "https://www.codebuddy.cn",
		WebBase:     "https://www.workbuddy.cn",
		AuthBase:    "https://www.codebuddy.cn",
		Origin:      "https://www.workbuddy.cn",
		UAPlatform:  "WorkBuddy",
		Domain:      "www.workbuddy.cn",
		Caps:        cnAllCaps,
	},
	"intl-cli": {
		ID:          "intl-cli",
		ChatBase:    "https://www.codebuddy.ai",
		BillingBase: "https://www.codebuddy.ai",
		WebBase:     "https://www.codebuddy.ai",
		AuthBase:    "https://www.codebuddy.ai",
		Origin:      "https://www.codebuddy.ai",
		UAPlatform:  "WorkBuddy AI",
		Domain:      "www.codebuddy.ai",
		Caps:        intlBillingOnly,
	},
	"intl-work": {
		ID:          "intl-work",
		ChatBase:    "https://www.workbuddy.ai",
		BillingBase: "https://www.workbuddy.ai",
		WebBase:     "https://www.workbuddy.ai",
		AuthBase:    "https://www.workbuddy.ai",
		Origin:      "https://www.workbuddy.ai",
		UAPlatform:  "WorkBuddy AI",
		Domain:      "www.workbuddy.ai",
		Caps:        intlBillingOnly,
	},
}

// defaultCNChatBase 迁移前的 CN chat 默认 host（也是 LegacyChatBase 的 cn 取值）。
// 用于区分「New() 的内置默认」与「用户显式配置的 upstream.cn_chat_base」：
// Client.ChatBaseCN 恒非空（New 里就填了默认），只看非空会把 cn-work 的 chat
// host 也压回 copilot.tencent.com —— 与契约表直接冲突。
const defaultCNChatBase = "https://copilot.tencent.com"

// defaultIntlChatBase 迁移前的 global chat 默认 host（= LegacyChatBase 的 global 取值）。
const defaultIntlChatBase = "https://www.workbuddy.ai"

// profilesActive 路由开关：只有 enabled=true 且 detect_only=false 才按 profile 路由。
func (c *Client) profilesActive() bool {
	return c != nil && c.ProfilesEnabled && !c.ProfilesDetectOnly
}

// ProfileFor 返回账号生效的产品 profile 描述符。
//
// ok=false 表示 profile 路由未激活（调用方必须走旧路径）。激活时返回的 ID 已应用
// CN 逃生门：global.enabled=false 时 intl-* 强制回落 cn-cli/cn-work（不得绕过）。
func (c *Client) ProfileFor(a *auth.Auth) (ProductProfile, bool) {
	if !c.profilesActive() || a == nil {
		return ProductProfile{}, false
	}
	id := effectiveProfile(a)
	// 逃生门：global.enabled=false（Client.GlobalEnabled 由 config 注入）时，
	// intl-* 一律按 cn 对应 profile 路由，否则纯 CN 部署会被显式 profile 绕过。
	if !c.GlobalEnabled && strings.HasPrefix(id, "intl-") {
		switch id {
		case "intl-cli":
			id = "cn-cli"
		default:
			id = "cn-work"
		}
	}
	desc, ok := productProfileTable[id]
	if !ok {
		// 未知值（手写 auth 文件）：按选择链末端兜底，绝不编造端点。
		desc = productProfileTable["cn-cli"]
	}
	return desc, true
}

// effectiveProfile 返回账号的 profile ID（① 选择链）。
//
// 生产路径上 a.Profile 已由 auth.Parse 按契约链算好；这里只在手工构造的 Auth
// （Profile 空）时用同一份判据补算，避免「测试/工具直连时静默落到 cn-cli」。
func effectiveProfile(a *auth.Auth) string {
	if a == nil {
		return "cn-cli"
	}
	if p := strings.TrimSpace(a.Profile); p != "" {
		return p
	}
	return auth.ResolveProfile("", "", a.RealmStored(), a.DomainValue())
}

// isIntlProfile 报告 profile 是否国际版（intl-*）。
func isIntlProfile(id string) bool { return strings.HasPrefix(id, "intl-") }

// intlEndpoint 报告该账号的端点是否应按「国际版」形态选择（路径族 / console 兜底 /
// 模型目录路径）。profile 激活时按 profile 的 realm 判（intl-cli 即使 stored realm
// 是 cn 也走国际版路径族）；未激活时逐字回落旧的 globalOn(a)。
//
// 注意：这与 globalOn(a) 不是同一个判据 —— globalOn 还管 global 模型探测/注册/trial
// 等 realm 语义，本函数只决定**端点路径族**，故两者并存。
func (c *Client) intlEndpoint(a *auth.Auth) bool {
	if desc, ok := c.ProfileFor(a); ok {
		return isIntlProfile(desc.ID)
	}
	return c.globalOn(a)
}

// legacyTaskOrigin ClaimReward 迁移前硬编码的 Origin/Referer 基础域
// （与 webBase 解耦：即便 WebBaseCN 被覆盖，历史实现仍写 workbuddy.cn）。
const legacyTaskOrigin = "https://www.workbuddy.cn"

// taskOrigin 成长任务领奖（ClaimReward）类端点的 Origin/Referer 基础域。
// profile 激活 → 表的 Origin 列（cn-cli 是 codebuddy.cn，与 web base 的
// workbuddy.cn 不同，这正是「profile Origin」必须独立存在的原因）；
// 未激活 → 迁移前硬编码值（逐字一致）。
func (c *Client) taskOrigin(a *auth.Auth) string {
	if desc, ok := c.ProfileFor(a); ok {
		return desc.Origin
	}
	return legacyTaskOrigin
}

// webOrigin 官网 web 类端点（ReportWebEvent）的 Origin 基础域。
// profile 激活 → 表的 Origin 列；未激活 → 旧的 webBase(a)（逐字一致）。
func (c *Client) webOrigin(a *auth.Auth) string {
	if desc, ok := c.ProfileFor(a); ok {
		return desc.Origin
	}
	return c.webBase(a)
}

// profileChatBase 按 profile 表 + ② host 覆盖轴返回 chat base。
func (c *Client) profileChatBase(desc ProductProfile) string {
	if isIntlProfile(desc.ID) {
		if c.ChatBaseGlobal != "" {
			return c.ChatBaseGlobal
		}
		return desc.ChatBase
	}
	// cn-*：upstream.cn_chat_base 覆盖（显式配置才生效，见 defaultCNChatBase 注释）。
	if c.ChatBaseCN != "" && c.ChatBaseCN != defaultCNChatBase {
		return c.ChatBaseCN
	}
	return desc.ChatBase
}

// profileBillingBase 按 profile 表 + ② host 覆盖轴返回 billing base。
// 只有 intl-* 可被 global.billing_base 覆盖；cn-* 恒取表值（无覆盖键）。
func (c *Client) profileBillingBase(desc ProductProfile) string {
	if isIntlProfile(desc.ID) && c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return desc.BillingBase
}

// legacyChatBase profile 化**之前**的 realm 默认 chat host：
// cn → copilot.tencent.com，global → www.workbuddy.ai。
//
// 它**不是** profile 的 ChatBase，也不受 upstream.cn_chat_base / global.chat_base
// 影响（契约 §1 原文只点名 cn_chat_base，但两者都是「覆盖」而非「旧默认」：
// 若覆盖值本身失效，回落必须落到真正的默认 host 才有意义）。
// realm 取**旧判据**（globalOn，即 stored realm + 逃生门），不是 profile 的 realm。
func (c *Client) legacyChatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultIntlChatBase
	}
	return defaultCNChatBase
}

// refreshBaseCandidates refresh 的 base 候选序列。
// profile 未激活 → 单元素（与迁移前逐字一致，零回归）。
func (c *Client) refreshBaseCandidates(a *auth.Auth) []string {
	if !c.profilesActive() {
		return []string{c.chatBase(a)}
	}
	authBase := c.profileAuthBase(a)
	if authBase == "" {
		return []string{c.chatBase(a)}
	}
	legacy := c.legacyChatBase(a)
	if authBase == legacy {
		return []string{authBase}
	}
	return []string{authBase, legacy}
}

// shouldFallbackRefresh 报告 refresh 错误是否应换下一个候选：
// 4xx（含 401/404 等）与网络/传输错误回落；5xx 与业务错误不回落。
// 与 billingMeterPaths 同款思路（只对「这个 host 不认」类错误换域），但契约 §1
// 明确要求 4xx + 网络错误，故比 ErrNotFound-only 更宽。
func shouldFallbackRefresh(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) {
		return true // 非 *Error = 传输层/解析错误
	}
	return ue.Status >= 400 && ue.Status < 500
}
