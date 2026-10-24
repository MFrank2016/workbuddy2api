// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone             ErrKind = iota // 成功
	ErrHardCredit                      // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                        // 429 软限流 → 短冷却
	ErrSessionDead                     // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                        // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                          // 5xx 上游故障
	ErrContentBlocked                  // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                       // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrAccountFault                    // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrModelBlocked                    // 11102「该后端无此模型」→ (账号,模型) 负缓存避让，切模型/切账号
	ErrWafBlock                        // 403 + 非业务信封体（APISIX WAF 拦截页/空体）→ 账号软冷却 + 抖动退避（WAF 403 修复 P0-1）
	ErrPromptTooLong                   // 11115「prompt is too long」→ 请求级错误（上下文超限是请求的问题非账号的问题）：不罚号、不轮转，末端透传原文
	ErrImageInvalid                    // 图片请求格式/数据无效 → 请求级错误：不罚号、不轮转，末端透传原文
	ErrUpstreamBusy                    // 6021「queue.full.title」渠道级容量满载 → 不罚号、不轮转，末端 503 + upstream_busy
	ErrModelEntitlement                // 11158「model not allowed for user type」（免费号不可用）→ 模型级权限，不罚号、仍轮换
	ErrClient                          // 其他 4xx / 业务错误
	ErrToolCallMismatch                // 11148 工具调用/结果历史错配 → 请求级错误：不罚号、不轮转
	// ErrRouteConfig 账号绑定的出口线路不可用（线路名不存在 / 地址非法 / 未配置线路）。
	// 这是**配置错误**，换多少个号都一样：不罚号、不轮转、不喂熔断，直接失败。
	// 此前它被当成普通 error 走「网络抖动」分支，一次拼错就能把整池打空并最终 503。
	ErrRouteConfig
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrAccountFault:
		return "account_fault"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrWafBlock:
		return "waf_block"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrUpstreamBusy:
		return "upstream_busy"
	case ErrModelEntitlement:
		return "model_entitlement"
	case ErrToolCallMismatch:
		return "tool_call_mismatch"
	case ErrRouteConfig:
		return "route_config"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// matchMode 描述错误分类 marker 的匹配通道。6 组词表 + 文案分类共用同一匹配器，
// 消除「小写 Contains + 原文 Contains 双通道」在 Classification 各分支的手写循环重复。
type matchMode uint8

const (
	// matchFold 大小写不敏感：lower(body) 含 lower(pat) 或 body 原字串含 pat。
	// 双通道与历史手写循环逐字等价（小写比较 + 中文原文比较）。
	matchFold matchMode = iota
	// matchExact 大小写敏感的字面包含。
	matchExact
	// matchLower 在 lower(body) 上做包含匹配（pat 须已小写）。
	matchLower
)

// errorRule 一条错误分类规则：命中 patterns 中任一 marker 即归类为 kind。
type errorRule struct {
	kind     ErrKind
	mode     matchMode
	patterns []string
}

// matchPattern 报告 body 是否命中单个 marker pattern。
func matchPattern(p string, mode matchMode, body, lower string) bool {
	switch mode {
	case matchFold:
		return strings.Contains(lower, strings.ToLower(p)) || strings.Contains(body, p)
	case matchExact:
		return strings.Contains(body, p)
	case matchLower:
		return strings.Contains(lower, p)
	default:
		return false
	}
}

// hit 报告 rule 是否命中 body（任一 marker 命中即真）。
func (r errorRule) hit(body, lower string) bool {
	for _, p := range r.patterns {
		if matchPattern(p, r.mode, body, lower) {
			return true
		}
	}
	return false
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	// RetryAfter 上游明示的等待时长（Retry-After 秒 / retry-after-ms /
	// x-ratelimit-reset 头解析，见 ParseRetryAfter）。零值 = 上游未明示，
	// 冷却时长回落调用方计算值。挂载点选在 Error 信封（任务书 P1-2「贴合的
	// 挂载点」）：Kind 决定「罚不罚」，RetryAfter 决定「罚多久」，同为上游
	// 响应的一等公民，与 Kind/Status/Msg 同居信封而非另开解析层。
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardRule 余额不足关键词（大小写不敏感 + 中文原文双通道）。
var hardRule = errorRule{kind: ErrHardCredit, mode: matchFold, patterns: []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}}

// softRateRule 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateRule = errorRule{kind: ErrSoftRate, mode: matchFold, patterns: []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}}

var sessionDeadRule = errorRule{kind: ErrSessionDead, mode: matchExact, patterns: []string{"Offline user session not found", "12153"}}

// contentBlockedRule 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedRule = errorRule{kind: ErrContentBlocked, mode: matchLower, patterns: []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}}

// badParamsRule 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsRule = errorRule{kind: ErrBadParams, mode: matchExact, patterns: []string{
	"Unmarshal chat params failed",
	`"code":11101`,
}}

// invalidImageRule 图片请求格式/数据无效（HTTP 400）的**文案**形态。这类错误由
// 请求内容决定，不是账号问题：换账号不会改变同一 body 的解析结果。上游常见形态包括
// `Parse message failed: invalid image_url content`、invalid_image_data、
// `replace the image`。
//
// 业务码 11135 不放在这里：code 判定必须容忍 JSON 空白（`"code": 11135`），
// 字面量 marker 只能覆盖紧凑形态，故统一走 codeMarker（见 Classify 的 400 分支）。
var invalidImageRule = errorRule{kind: ErrImageInvalid, mode: matchFold, patterns: []string{
	"invalid image_url content",
	"invalid_image_data",
	"replace the image",
}}

// alreadyCheckinRule "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinRule = errorRule{mode: matchFold, patterns: []string{"已签到", "already"}}

// inactiveCheckinPatterns 签到活动「没开/已关」的文案形态。**这不是全局 errorRule**：
// 它不参与 Classify（活动状态不是错误分类，不该影响 pool 冷却/熔断），唯一调用点
// 是 scheduler 的签到分支（经 IsInactiveCheckin）。
//
// 文案来源：上游对「活动不在进行中」的签到请求返回业务错误（code 10001 等），实测
// 与公开反馈出现的措辞集中在三类：
//   - 活动未开启 / 未开始 / 未开放 / 已结束 / 已关闭 / 已暂停
//   - 签到（活动）已过期
//   - 当前无（进行中的）活动
//
// 负向守卫（见 IsInactiveCheckin）：文案必须同时提到「活动」或「签到」，否则
// `token 已过期` / `session 已过期` 这类**凭证**失效会被误判成活动结束——那会把
// 真实的账号故障静默记成正常态。
var inactiveCheckinPatterns = []*regexp.Regexp{
	regexp.MustCompile(`活动.*(?:未开启|未开始|未开放|已结束|已关闭|已暂停)`),
	regexp.MustCompile(`(?:签到|活动).*已过期`),
	regexp.MustCompile(`当前无.*活动`),
}

// isQueueFullBody 判定渠道级容量满载文案（6021 queue.full.title 家族）。
// 该形态表明上游推理集群排队队列打满，与具体账号无关；换号在任何时刻都会撞
// 同一堵墙，正确处置是不罚号不轮转（见 Classify 的 6021 分支）。
func isQueueFullBody(lower string) bool {
	return strings.Contains(lower, "queue.full") || strings.Contains(lower, "queue full")
}

// accountFaultRule 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控
//     （实测 global 账号：同一规范化请求 A1 403/11140 vs A2 429/14017，差异全由账号
//     数据决定）。需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（上方 softRateRule
// 先命中）。故此处只收 msg 关键词 "request illegal"（auth_forbidden 的真实文案），
// 120 与 private 均落同一分类。14017 文案唯一（无软限流歧义），可安全收录。
// channelVerificationMarker 上游「渠道校验失败」的稳定标记（出现在 displayTips 文案）。
//
// 根因（实测）：渠道校验失败与账号封禁（11140）的 msg **同为** "request illegal"，
// 但渠道校验失败是**请求侧**问题（网关缺会话头族/缺 system，可修复），其
// displayTips 含这句稳定英文；账号封禁 body 不含此句。只按 msg "request illegal"
// 匹配会把渠道失败误归 ErrAccountFault → handler 按 "request illegal" 把**健康账号
// Disable**（实测 8 个 global 号因此被误禁）。故在 accountFaultRule 之前按此标记分流。
const channelVerificationMarker = "channel verification"

// accountFaultRule 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控
//     （实测 global 账号：同一规范化请求 A1 403/11140 vs A2 429/14017，差异全由账号
//     数据决定）。需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（上方 softRateRule
// 先命中）。故此处只收 msg 关键词 "request illegal"（auth_forbidden 的真实文案），
// 120 与 private 均落同一分类。14017 文案唯一（无软限流歧义），可安全收录。
var accountFaultRule = errorRule{kind: ErrAccountFault, mode: matchFold, patterns: []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
}}

// promptTooLongRule 11115「prompt is too long」判定（任务书 prompt-too-long §1）。
// 定位：上下文超限是**请求的问题不是账号的问题**——同一个 body 换任何账号发都会
// 超限，与 WAF fail-fast 同哲学（确定与账号无关的错误不罚号不轮转，白白浪费健康号
// 的请求配额）。marker 双通道：
//   - `"code":11115`：业务信封 code 字段（JSON 空格容差，与 11102/6004 的 code 判定
//     同形态；`"code":"11115"` 字符串形态也命中）；
//   - "prompt is too long"：msg 文案（大小写不敏感）。
//
// 只在 400/404/413 请求级状态码上判（429+11115 概率极低且属限流语义优先，
// 5xx 属服务端故障优先）——与 IsModelBlocked 的 400/404 口径同理。误判代价
// （好 body 被归 prompt_too_long）：不罚号 + 不轮转 + 透传原文，客户端看到
// 上游原文可自行排查，代价可控。
var promptTooLongRule = errorRule{kind: ErrPromptTooLong, mode: matchFold, patterns: []string{
	`"code":11115`,
	`"code":"11115"`,
	"prompt is too long",
}}

// isPromptTooLongStatus 11115 只在请求级 4xx 上判（见 promptTooLongRule 注释）。
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetPattern 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetPatternCN = `将在 (.+?) 重置`
const softRateResetPatternEN = `(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`

// 限流判定正则预编译为包级 var（发现 8）：IsModelRateLimit / ParseRateReset
// 在每次错误分类、每个限流 body 上调用，函数体内 MustCompile 是纯浪费；
// 错误风暴（429 轰炸）时尤甚。模式串均为纯常量，与 sanitize.go 的包级
// 预编译先例保持一致。regexp 并发安全（匹配只读），无需额外锁。
var (
	reModelRateLimit  = regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	reSoftRateResetCN = regexp.MustCompile(softRateResetPatternCN)
	reSoftRateResetEN = regexp.MustCompile(softRateResetPatternEN)
)

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	return reModelRateLimit.MatchString(body)
}

// modelBlockCode 明确指向「该后端无此模型」的业务 code（reference converter.MODEL_NOT_SERVABLE_CODES）。
const modelBlockCode = "11102"

// modelBlockMsgMarker 11102 答复的确定性文案（官方 error message 固定短语）。
// 只收这个窄短语，不收 "model ... not found" 宽正则——后者会误伤其他业务的 not found 措辞
// （reference maiphucgiang 报告提的「11102 撞在 ID 上」坑的同类问题：宁缺毋滥）。
const modelBlockMsgMarker = "service info not found"

// ModelBlockReason 11102 负缓存条目在 pool.modelCooldowns 里的 reason 前缀。
// handler 写 BlockModelBackoff；pool.BlockModelClear 按 "11102" 前缀识别条目
// （与 6004 条目的 "6004 model rate limit" reason 互不干扰，两者共存于同一 map 键）。
const ModelBlockReason = "11102 model not available"

// IsModelBlocked 报告 body 是否是「该后端无此模型」(11102) 的确定性答复。
//
// 只比对 code/msg 等独立字段，绝不做整段文本子串匹配：错误体还带 requestId 等字段，
// 拿整段文本匹配会把 "11102" 撞在 ID 上、误避让一个本来能用的模型（reference
// converter._parse_not_servable 的坑，app/model_blocks.py:408-411 讨论）。
// 判定 = code 字段精确等于 "11102"，或 msg/message 字段命中窄短语 "service info not found"
// （两者任一命中即真）。只看 400/404：429 带 11102 属限流语义（不在此判定范围）。
// 字段遍历覆盖顶层与 error 子对象两层（对齐 converter 的 nodes 收集口径）。
func IsModelBlocked(status int, body string) bool {
	if (status != http.StatusBadRequest && status != http.StatusNotFound) || body == "" {
		return false
	}
	// 轻量预检：body 既无 "11102" 又无 marker 时直接短路（大多数 4xx 零分配返回）。
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(strings.ToLower(body), modelBlockMsgMarker) {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}
	code, msg := "", ""
	for _, node := range nodes {
		for _, key := range []string{"code", "errCode", "error_code"} {
			if v, ok := node[key]; ok && v != nil && code == "" {
				code = strings.TrimSpace(fmt.Sprint(v))
			}
		}
		for _, key := range []string{"msg", "message"} {
			if v, ok := node[key].(string); ok && v != "" && msg == "" {
				msg = strings.TrimSpace(v)
			}
		}
	}
	if code == modelBlockCode {
		return true
	}
	return strings.Contains(strings.ToLower(msg), modelBlockMsgMarker)
}

// hasBusinessCode reports whether a JSON error envelope contains an exact
// business code in a field named "code". Upstream envelopes vary between
// top-level and nested error/data objects, so walk the decoded structure.
func hasBusinessCode(body, want string) bool {
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(value any) bool {
		switch node := value.(type) {
		case map[string]any:
			if code, ok := node["code"]; ok && strings.TrimSpace(fmt.Sprint(code)) == want {
				return true
			}
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}

// hasBusinessEnvelope 报告错误 body 是否携带上游业务信封形态（JSON 且含
// `"code":` 或 `"msg":` 字段）。WAF 403 判定（IsWafBlocked）用「无业务信封」
// 区分 APISIX WAF 拦截页（HTML/空体/纯文本）与上游业务层 403（带 code/msg
// 信封，正常走既有分类）。JSON 解析不做：信封存在性只需字段名命中——
// 畸形 JSON 但含 `"msg":` 字样仍按业务响应保守处理（宁漏判 WAF 也不误罚
// 业务 403，后者有各自的权威分类）。
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`)
}

// IsWafBlocked 报告 403 响应是否为 WAF 拦截形态（任务书 P0-1 判定口径）：
// HTTP 403 且 body 无业务信封（无 `"code":`/`"msg":` JSON 字段——HTML 拦截页、
// 空体、纯文本均命中）。带业务信封的 403（11140 request illegal / 11128 等）
// 仍走既有分类链，不受影响。403 含 accountFault 文案的维持现状
// （ErrAccountFault → Disable），由 Classify 的规则序保证（本函数仅作形态
// 判定，不重复关键词逻辑）。
func IsWafBlocked(status int, body string) bool {
	return status == http.StatusForbidden && !hasBusinessEnvelope(body)
}

// retryAfterHeaderCandidates 冷却时长优先解析的响应头候选序列（P1-2，对齐
// intl CLI parseRetryAfterMs / parseRateLimitResetMs 的头族）：
// retry-after（秒，RFC 7231）/ retry-after-ms（毫秒）/ x-ratelimit-reset
// （epoch 秒或毫秒，取 now+ 剩余量）。大小写不敏感（http.Header.Get 已归一）。
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity 解析结果的上限（超过视为上游异常值丢弃，回落本地计算），
// 与 pool 的 softRateMax 默认 2h 同量级（上游不该明示比冷却封顶更长的等待）。
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter 从限流/拦截响应头解析上游明示的等待时长（P1-2）：
// 依次尝试 Retry-After（整数秒）→ retry-after-ms（整数毫秒）→
// x-ratelimit-reset（纯数字按 epoch 秒/毫秒推断，HTTP-Date 形态不支持——
// 上游族实践发的是数字）。任一头缺失/非法/非正/超上限则尝试下一头；
// 全部不可用返回 false（调用方回落既有计算值，绝不臆造等待时长）。
// 语义对齐 intl CLI（parseRetryAfterMs / parseRateLimitResetMs，报告 §2.1），
// 上游族会发这些头是其存在依据。
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if !isAllDigits(v) {
			continue // 非纯数字（如 HTTP-Date）不解析，宁缺毋滥
		}
		n, ok := parseRetryNumber(v, name)
		if !ok {
			continue
		}
		if n <= 0 || n > retryAfterSanity {
			continue // 非正/异常大：丢弃（回落本地计算）
		}
		return n, true
	}
	return 0, false
}

// isAllDigits 报告 s 是否为纯数字（前置快筛，免 strconv 之后再判语义）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseRetryNumber 按头名口径把纯数字串折算成时长。x-ratelimit-reset 是
// epoch 时刻而非时长：秒口径（10 位）与毫秒口径（13 位）都按「now+ 该时刻
// 的剩余量」折算，已在过去则不可用。位数不足（8 位以下）无法判定 epoch
// 语义的丢弃（宁缺毋滥：x-ratelimmit-reset 族实践发 epoch，短串多半是
// 序号之类的误用头）。
func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	// 上限 16 位防 int64 溢出（超过 epoch 毫秒的现实量级必非法）。
	if len(v) > 16 {
		return 0, false
	}
	var n int64
	for _, r := range v {
		n = n*10 + int64(r-'0')
	}
	switch headerName {
	case "Retry-After":
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		return time.Duration(n) * time.Millisecond, true
	default: // X-Ratelimit-Reset：epoch → 剩余量
		sec := n
		if len(v) >= 12 { // 毫秒口径（13 位）；11 位边界按秒（误判代价是多算 1000 倍）
			sec = n / 1000
		}
		remain := time.Until(time.Unix(sec, 0))
		return remain, true
	}
}

// ParseRateReset 从任何限流响应 body 里统一解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
//
// 是否走模型级豁免、时日对齐到 until 还是 modelCooldowns，由冷却决策侧（pool）按
// IsModelRateLimit 判定，本函数只负责「把上游明说的恢复时刻抽出来」。没有时间文案
// 的限流也照常由调用方退回有界退避（绝不臆造时间）。
func ParseRateReset(body string) (time.Time, bool) {
	m := reSoftRateResetCN.FindStringSubmatch(body)
	if len(m) < 2 {
		m = reSoftRateResetEN.FindStringSubmatch(body)
	}
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  0. 11102（IsModelBlocked）——「该后端无此模型」确定性答复，语义最具体，最先判
//     （详见 IsModelBlocked 注释；只认 400/404，429+11102 属限流语义走第 3 层）。
//  1. 402 —— 真正的计费余额耗尽状态码，最严、最不可自愈，最先判。
//  2. sessionDeadRule —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. accountFaultRule —— 账号级授权/配额故障（11140 request illegal auth 风控、
//     14017 trial not activated register 未完成）。与 429 一起纳入轮换冷却，且必须
//     先于 status==429 判定：14017 常带 429 状态码，若落到 status==429 会误归
//     soft_rate（"限流"语义不符：限流可指数退避等自愈，账号级故障等不来）。
//     11140 的 model 级限流变体（rate-limiting 文案）因 marker 不含该文案而天然
//     不在此层命中，后续走 softRateRule 层，不受影响。
//  4. 429 + code 14018 —— 明确的账号积分耗尽，归 ErrHardCredit（issue #175）。
//     只认结构化业务码，不靠可能跨计费/限流两界的文案猜测。
//  5. status==429 —— 限流状态码兜底（本层先于 hardRule，fork-scan-absorb T-3）：
//     429 body 高频携带 "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，
//     若 hardRule 先判会把限流误归 ErrHardCredit 硬冷却到次日 04:00，白扔号约
//     12h。状态码是比关键词更权威的信号：上游既然给了 429，就按限流语义处理
//     （宁可短冷却自愈，不可长冷却弃号）；真正的余额耗尽由 402（第 1 层）或
//     14018（第 4 层）捕获，
//     非 429 状态码的 quota 措辞仍走下方 hardRule（第 5 层）。
//  6. hardRule —— 非 429 响应携带计费关键词（200 业务信封 / 403 信封等）。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit；429 场景已由
//     第 5 层前置接管（issue #28 记录的非 429 反向误判风险保持原样，待上游
//     原始响应确认后再定）。
//  7. softRateRule —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时已被第 5 层
//     短路，结果同为 soft_rate。
//  8. 11115 —— 「prompt is too long」请求级语义：判在 404/5xx 与通用 4xx 兜底
//     之前（404 上打 11115 若落 ErrNotFound 会误冷却账号——上下文超限与账号无关）。
//  9. 404 / 5xx —— 与限流无关的常规分类。
//  10. IsWafBlocked —— 403 且无业务信封（HTML 拦截页/空体/纯文本）：APISIX WAF
//     拦截形态（WAF 403 修复 P0-1）。判在通用 4xx 兜底**之前**：此前该形态落
//     ErrClient → applyErrorPolicy 只换号不罚 → 连环 403（报告 §4.1 的根因）。
//     带业务信封的 403 已被上方各层捕获（11140 request illegal →
//     ErrAccountFault 禁用语义不变），走不到本层。
//  11. 内容策略/参数错误/其他 4xx —— 通用兜底。
func isToolCallMismatch(lower string) bool {
	return strings.Contains(lower, "tool calls and tool results do not match") ||
		strings.Contains(lower, "tool_call_sequence_broken") ||
		codeMarker(lower, "11148")
}

func Classify(status int, body string) ErrKind {
	// 11102「该后端无此模型」须最先判：它是「模型在后端不存在」的确定性答复，语义比
	// 计费/限流都更具体——若不先判，msg 里的 "service info not found" 虽不含余额词、
	// 但可能被更宽的 4xx 兜底归为 ErrClient（只换号不避让），该坏号会留在池内反复被选中。
	// 先于 hardRule：11102 答复的 msg 是模型不存在，不含 credit/quota/积分 等计费词，
	// 正常不会撞 hardRule，但前置判定让语义零歧义（防上游未来在 msg 里混入余额词）。
	// 只认 400/404（见 IsModelBlocked），429+11102 落下方 status==429 层走限流语义。
	if IsModelBlocked(status, body) {
		return ErrModelBlocked
	}
	// 11158「model not allowed for user type」（免费账号不可用）：模型级权限，不是
	// 账号故障——不罚号、仍轮换（付费号可能可用）；由 handler 记录观测供 /status 展示。
	if hasBusinessCode(body, "11158") {
		return ErrModelEntitlement
	}
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	// sessionDead / accountFault 先于 status==429（原顺序已如此，此处只是跟随
	// 429 前移保持相对次序）：账号级终态等不来自愈，限流状态码不得掩盖它们
	// （429+14017 必须 accountFault，401+12153 混排 "rate limit" 必须 sessionDead）。
	if sessionDeadRule.hit(body, lower) {
		return ErrSessionDead
	}
	// 渠道校验失败先于 accountFaultRule（根因修复，实测 8 个 global 号被误禁）：
	// 上游「渠道校验失败」与「账号封禁」（11140）的 msg **同为** "request illegal"，
	// 但渠道失败是**请求侧**问题（网关缺会话头族/缺 system，可修复），其 displayTips
	// 含稳定标记 channelVerificationMarker；账号封禁 body 不含。若不先分流，渠道失败
	// 会被 accountFaultRule 按 msg 归 ErrAccountFault → handler 按 "request illegal"
	// 把健康账号 Disable。归 ErrBadParams（请求级：不罚号不熔断不喂连败，但仍轮转
	// ——换号可能换到不同模型权限，值得再试）。
	if strings.Contains(lower, channelVerificationMarker) {
		return ErrBadParams
	}
	if accountFaultRule.hit(body, lower) {
		return ErrAccountFault
	}
	// 14018 是明确的账号积分耗尽业务码。它必须先于通用 429 兜底，否则会被误判为
	// 可自愈的软限流并在全池冷却时反复兜底选中（issue #175）。仅按结构化 code
	// 判定；无该 code 的 "credits exhausted" 文案仍保持普通 429 的软限流语义。
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return ErrHardCredit
	}
	// 6021「queue.full.title」是渠道级容量满载，必须先于通用 429 兜底：
	// 按限流逐号软冷却会在数秒内把整池健康号误杀，且换号也撞同一堵墙。
	if (status == http.StatusTooManyRequests && hasBusinessCode(body, "6021")) || isQueueFullBody(lower) {
		return ErrUpstreamBusy
	}
	// status==429 先于 hardRule（fork-scan-absorb T-3）：限流响应 body
	// 高频携带 "quota exceeded"/"额度不足" 等跨两界措辞，hardRule 先判会误归
	// ErrHardCredit 硬冷却到次日 04:00。402 真余额在上层已判；非 429 的 quota
	// 措辞仍走下方 hardRule，历史语义不变。
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if hardRule.hit(body, lower) {
		return ErrHardCredit
	}
	if softRateRule.hit(body, lower) {
		return ErrSoftRate
	}
	// 11115「prompt is too long」（任务书 prompt-too-long §1）：判在 404/5xx/
	// WAF/内容策略/参数错误/通用 4xx 之前——请求级语义最具体（上下文超限），须
	// 先于宽泛的状态码兜底（404 兜底会误归 ErrNotFound 只冷却不透传；ErrClient
	// 只换号，浪费健康号配额）。只认请求级 4xx 状态码（见 promptTooLongRule），
	// 429/5xx 在上方已被各自状态码层短路（限流/服务端故障语义优先）。
	if isPromptTooLongStatus(status) && promptTooLongRule.hit(body, lower) {
		return ErrPromptTooLong
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// WAF 403（无业务信封的拦截形态）：判在内容策略/参数错误/通用 4xx 之前——
	// 这些层只认带文案的 body，WAF 空体/HTML 永远不会命中它们的 marker，
	// 但落 ErrClient 兜底的代价是「只换号不罚」（报告根因），必须在兜底前分流。
	// 带信封的 403 在上方各层已有权威分类，不受影响。
	if IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	// 图片格式/数据错误是确定性的请求级错误：同 body 换账号结果不变，直接
	// fail-fast，避免把健康账号轮转一遍后仍把最终 503 返回给客户端。
	// 业务码 11135 经 codeMarker 而非字面量 marker：上游 JSON 含空白
	// （`"code": 11135`）时字面量 marker 会漏判，导致退化成 ErrClient 并继续轮转。
	// 口径与 hint.go 的 isInvalidImageData（同样用 codeMarker）一致。
	if status == http.StatusBadRequest && (invalidImageRule.hit(body, lower) || codeMarker(lower, "11135")) {
		return ErrImageInvalid
	}
	// 11148 is deterministic for this request body, so switching accounts cannot fix it.
	if status == http.StatusBadRequest && isToolCallMismatch(lower) {
		return ErrToolCallMismatch
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
	// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
	// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
	// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
	// 模型权限，值得再试一次）。
	if status >= 400 {
		if contentBlockedRule.hit(body, lower) {
			return ErrContentBlocked
		}
		if badParamsRule.hit(body, lower) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardRule 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 键按 realm 分层（map[realm]map[model][]efforts）：CN 探测结果不得被 global 同模型名
	// 请求复用（同名不同档位会错误降级，C-2）。global 侧暂无 efforts 探测 → 桶缺失即透传。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string

	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string

	// globalModels 缓存 global 模型名目录纯动态探测结果（1h TTL + 5min 负缓存），
	// 见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints atomic.Bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认 WorkBuddy
	// 三段式与 billingUA 单段式）。空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing/checkin 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空，见 billingUA）。
	// issue #42 深挖：官网「使用端」列基于出站请求的 UA/X-Product 服务端归因，
	// 官方 WorkBuddy 桌面 UA 见 defaultWorkBuddyUAFor。默认值已对齐官方（A 段变更），
	// 用户仍可显式配置完全自定义的 UA。
	UserAgent string

	// DeviceToken 设备风控 Token（X-Device-Token 头）兜底来源：config upstream.device_token。
	// 仅当 auth.Auth.DeviceToken 为空时才取此值；两者皆空则不注入该头。
	// 容器内无桌面端 Turing SDK，这是把外部（宿主/桌面端）生成的 token 注入的入口。
	// 另见 DeviceTokenFile 缓存读取：宿主可把 token 落 /app/data/device_token 共用。
	DeviceToken string

	// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
	// 读取频率限 5 分钟一次缓存（见 device_token.go），>1KB 或读失败则忽略。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceTokenFile string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空（默认）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
	// 见 injectAttribution / attributionClientName）。显式配 "SaaS" 还原旧行为
	// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
	ClientName string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + B 段的
	// X-IDE-Version）。空 = 内置默认 defaultClientVersion（对齐官方 5.5.4 分发包）。
	// config upstream.client_version 覆盖。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认 defaultCliVersion
	// （对齐官方内置 CLI 2.137.1）。config upstream.cli_version 覆盖。
	CliVersion string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 参数传入 ChatStream，
	// 由 ChatHeaders 注入（不再挂共享字段，杜绝并发串扰）。
	PassthroughIP bool

	ChatBaseCN    string
	BillingBaseCN string

	// WebBaseCN 官网域基址（任务领奖类接口走 www.workbuddy.cn；空 = 内置默认）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 即显式逃生门：即使用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool

	// ProfilesEnabled / ProfilesDetectOnly 产品 profile 路由开关（config profiles.*，
	// 缺省 false/true = 全关）。**只有 Enabled=true 且 DetectOnly=false 才按
	// productprofile.go 的表路由**；其余组合逐字走旧的 chatBase(a)/globalOn(a)。
	ProfilesEnabled    bool
	ProfilesDetectOnly bool
	// Retry 聊天 SSE 的同账号瞬时重试配置（见 retry.go）。零值 = 关闭，
	// 行为与现状逐字一致（5xx/传输失败直接换号轮转）。wiring 从 config 注入。
	Retry RetryConfig
	// modelRates 各 realm 的模型生效倍率原文缓存（panel 模型视图用；与 efforts 共用锁）。
	modelRates map[string]map[string]string
}

// New 生产默认值。Transport 由 newTransport() 集中构造（连接层加固：禁 h2 /
// TLS 握手超时 / 短 keepalive 探测，参数见 transport.go）。
func New() *Client {
	tr := newTransport()
	c := &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:      &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		ChatBaseCN:    "https://copilot.tencent.com",
		BillingBaseCN: "https://www.codebuddy.cn",
	}
	c.SanitizeFingerprints.Store(true)
	return c
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// clientFor 返回该账号出站应使用的 client（issue #210）。
//
// 未绑线路（ProxyName 为空）→ 走原来的共享 client，行为一字不变。
// 绑了线路 → 换成按线路缓存的 Transport（连接池分开，出口不串）。
// 线路名未知 → 返回 error，**不静默直连**：静默直连等于「以为隔离了其实没有」。
func (c *Client) clientFor(a *auth.Auth, chat bool) (*http.Client, error) {
	name := ""
	if a != nil {
		name = strings.TrimSpace(a.ProxyName)
	}
	if name == "" {
		if chat {
			return c.chatHTTP(), nil
		}
		if c.HTTP != nil {
			return c.HTTP, nil
		}
		return &http.Client{Timeout: 120 * time.Second, Transport: newTransport()}, nil
	}
	tr, err := TransportFor(name, a.SessionName())
	if err != nil {
		label := ""
		if a != nil {
			label = logfmt.Label(a.UID, a.Nickname) + " "
		}
		// **typed 错误**：这是配置问题，不是账号/传输问题。返回普通 error 会让
		// handler 走进「网络抖动」分支（NoteFailures + fail + 换号），一次拼错
		// 就能把整池打空并最终 503。ErrRouteConfig 在 handler 里是「不罚号、不轮转」。
		return nil, &Error{
			Kind:   ErrRouteConfig,
			Status: http.StatusServiceUnavailable,
			Msg:    fmt.Sprintf("账号 %s出口线路不可用：%v", label, err),
		}
	}
	// 复制客户端级设置（Timeout / CheckRedirect / Jar …），只换 Transport。
	// 直接 new 一个 client 会丢掉 upstream.timeout_seconds 等配置。
	base := c.HTTP
	if chat && c.ChatHTTP != nil {
		base = c.ChatHTTP
	}
	if base == nil {
		base = &http.Client{}
	}
	hc := *base
	hc.Transport = tr
	if chat {
		// 聊天无总时长（SSE 长流），首字节由 Transport 的 ResponseHeaderTimeout 管
		hc.Timeout = 0
	}
	return &hc, nil
}

// chatHTTPFor 聊天用 client（按账号线路）。
func (c *Client) chatHTTPFor(a *auth.Auth) (*http.Client, error) {
	return c.clientFor(a, true)
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

// chatBase 生效的 chat base。
//
// profiles 激活（enabled=true 且 detect_only=false）→ productprofile 表 + ② host 覆盖轴；
// 其余组合逐字走迁移前的旧判据（globalOn → globalChatBase / ChatBaseCN）。
func (c *Client) chatBase(a *auth.Auth) string {
	if desc, ok := c.ProfileFor(a); ok {
		return c.profileChatBase(desc)
	}
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// 显式传 realm 使 effort 降级按域取桶：CN 探测信息不得作用到 global 请求（C-2）。
// conversationID 为网关解析出的会话标识（用于 prompt_cache_key 注入的会话段；
// body 里自带 conversation_id 时以 body 为准）。uid8 来自账号 UID，是跨账号硬隔离段。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		// global 域降级源 = 远端探测桶（权威）∪ 产品静态兜底表（全局 21 名内档位如
		// deepseek-v4.1-flash ['high']）。当前探测桶为空时也按静态表降级，不全程透传
		// （issue #84：往 WorkBuddy 上游发 low/max 非法，须降级到 high）。
		efforts, defs = globalEffortMap(efforts, defs)
	}
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints.Load(), efforts, defs)
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	body = InjectPromptCacheKey(body, uid, conversationID)
	return body
}

// effortsSnapshot 返回指定 realm 的 effort 能力缓存副本；该域无探测 → nil（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

// storeEfforts 按 realm 写入 effort 能力缓存桶（efforts + defaultEfforts），并发安全。
// 供 CN FetchModels 与 global 探测共用：拉取到的模型档位落桶后，出站请求体 normalizeReasoningEffort
// 才能按域降级。efforts 与 defs 均空时删除该 realm 桶（等价「该域无可降级档位」）。
// 调用方负责在「无新数据」时跳过写（CN 侧空桶不清既有桶，见 FetchModels 尾部）。
func (c *Client) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	k := realmKey(realm)
	if len(efforts) == 0 && len(defs) == 0 {
		delete(c.efforts, k)
		delete(c.defaultEfforts, k)
		return
	}
	c.efforts[k] = efforts
	c.defaultEfforts[k] = defs
}

// GlobalEffortSnapshot 导出 global 域 effort 能力缓存（探测下发 ∪ 静态兜底合并后的桶），
// 供 /v1/models 输出 reasoning_supported_efforts / reasoning_default_effort。
// 返回副本；桶未填充（无 global 账号或从未探测）→ nil（调用方回落静态兜底表）。
func (c *Client) GlobalEffortSnapshot() (efforts map[string][]string, defaults map[string]string) {
	return c.effortsSnapshot("global"), c.defaultEffortsSnapshot("global")
}

// billingBase 生效的 billing base。
//
// profiles 激活 → 表值；只有 intl-* 可被 global.billing_base 覆盖（cn-* 无覆盖键）。
// 未激活 → 旧判据逐字（globalOn → globalBillingBase / BillingBaseCN）。
func (c *Client) billingBase(a *auth.Auth) string {
	if desc, ok := c.ProfileFor(a); ok {
		return c.profileBillingBase(desc)
	}
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
//
// profiles 激活 → 恒取表的 web base 列（**不受任何 override 影响**：cn-cli 的
// web base 固定 www.workbuddy.cn）。未激活 → 旧判据逐字。
func (c *Client) webBase(a *auth.Auth) string {
	if desc, ok := c.ProfileFor(a); ok {
		return desc.WebBase
	}
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（R9：国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm（profiles 激活时按 profile 的 realm，见 intlEndpoint）
// 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.intlEndpoint(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.intlEndpoint(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// body 读失败（连接中断/空闲掐流/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚（传输层故障不该喂熔断误罚号）。
func (c *Client) doJSON(a *auth.Auth, req *http.Request) (json.RawMessage, error) {
	client, cerr := c.clientFor(a, false)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// refreshIOTimeout 单次 refresh 网络调用的总时长上限。
// 远小于 HTTP.Client.Timeout(120s)：refresh 持锁窗口内做网络 I/O，超时越短，
// 单账号 hang 对该账号相关操作的阻塞越短（issue:持锁 120s I/O → 池级停滞）。
const refreshIOTimeout = 30 * time.Second

// refreshTokenExpiresInMax refresh 响应 expiresIn 的量级上限（10 年，纯防御值：
// 实测 R-D 响应恒 5184000=60d）。超限视为上游脏数据，不写 ExpiresAt（保留旧值），
// 防止 NeedsRefresh 永假导致 token 永不刷新反而真过期失效。
const refreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 并发安全模型（两段式，缩小持锁窗口）：
//   - 锁内仅做「读 refreshToken 快照」与「校验未变后写回新 token」两小段内存操作；
//   - 网络 I/O（doJSON）在**锁外**执行，带 30s ctx 超时——避免上游 hang 时长时间
//     独占 a.mu，阻塞同账号的 SaveAtomic / 其他刷新（issue:持锁 120s I/O）。
//   - 写回前重新校验快照一致性：若锁外期间另一 goroutine 已完成刷新（refreshToken
//     已变），本次结果直接采用（新 token 已生效），不再重复写回。
func (c *Client) RefreshToken(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
		ProxyName:    a.ProxyName,
		// 必须带上会话名：否则续期请求会按昵称 slug 渲染 {account}，而业务流量
		// 用的是存下来的 proxy_session —— 同一账号的续期与聊天从不同出口 IP 出去。
		ProxySession: a.ProxySession,
	}
	a.Unlock()

	// base 候选序列：profiles 未激活 = 单元素旧 chatBase（零回归）；激活 =
	// [AuthBase, LegacyChatBase]（契约 §1：refresh 一律用 auth base，4xx/网络错误回落）。
	// 每个候选单独构造请求（RefreshHeaders 会写头，请求对象不可复用）。
	var data json.RawMessage
	var err error
	candidates := c.refreshBaseCandidates(a)
	for i, base := range candidates {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/plugin/auth/token/refresh", nil)
		if rerr != nil {
			return rerr
		}
		c.RefreshHeaders(req, &hdrSnapshot)

		// 网络 I/O（锁外，30s 上限）。
		data, err = c.doJSON(&hdrSnapshot, req)
		if err == nil {
			break
		}
		if i < len(candidates)-1 && shouldFallbackRefresh(err) {
			log.Printf("WARN: [upstream] refresh acct=%s: %s failed (%v) — falling back to %s",
				logfmt.Label(a.UID, a.Nickname), base, err, candidates[i+1])
			continue
		}
		return err
	}
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// 第 2 段（锁内）：校验快照一致后写回。
	a.Lock()
	defer a.Unlock()
	// 写回守卫是 AND 语义：锁外期间另一刷新已完成 → 两 token 必同时变化（实测 R-D：
	// refresh 响应 accessToken/refreshToken 总是一起 rotate，写回也同时写两个），AND
	// 即「并发刷新已完成」判据；AND 与 OR 在真实形态下等价。唯 OR 会额外放弃的
	// 「只有单 token 变化」（如手工只改 auth 文件一个字段）不构成放弃条件——本次
	// 结果覆盖手工编辑。
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （实测 R-E：服务端无 rotation 撤销，并发双刷新拿到的两个新 token 都有效，
		// 后写覆盖先写二者等价可用；提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	// 实测 R-D 响应恒带 expiresIn=5184000（60d）——缺省分支仅为防御，保留旧值
	// 避免过期判定漂移。同理，超过 10 年的 expiresIn 按脏值处理保留旧值：
	// 实测 JWT exp-iat 与 expiresIn 严格自洽（R-F），超量级值只会是上游脏数据，
	// 照写会把 ExpiresAt 推到荒谬未来 → NeedsRefresh 永假 → token 永不刷新
	// 反而真过期失效。
	if tok.ExpiresIn > 0 && time.Duration(tok.ExpiresIn)*time.Second < refreshTokenExpiresInMax {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// 路径常量：CN 与 global 共用的 chat 出站路径（/v2 单路径）。
const chatCompletionsPath = "/v2/chat/completions"

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// DeptestOnly: 全库仅 upstream 包测试引用；生产全走 ChatStreamContext
// （handler 传 r.Context()）。迁 export_test.go 不可行——测试需要真实
// HTTP 回放走完整 chatPaths/monitorBody 链路，与生产共用同一实现。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 新调用方应优先用 ChatStreamContext 传入请求 ctx（客户端断连即中断在途调用、释放租约）。
//
// global chat 自 #119 实测后固定走 /v2（/console 挂腾讯云 WAF body 内容规则，
// 反引号 printf/whoami 等命令执行特征确定性 403；/v2 同 base 不挂该规则，实测等价端点）。
// 已知取舍：若上游未来关闭 /v2，global chat 将整体不可用——届时应重新启用 /console
// 路径（含 WAF 特征中和，见 issue #119 / pr162-review.md）。本注释即"坏了再说"的锚点。
// cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但从 ctx 派生请求 context：调用方（handler）传入
// r.Context() 后，客户端断连/请求取消会立即中断在途上游调用、释放连接与账号在途名额，
// 不再空转到 IdleTimeout。ctx 为 nil 时回落 Background。
//
// 错误路径（≥400 且非 fallback 状态码）除 (status, respBody) 外还返回**已分类的**
// *Error（Kind 信封 + Retry-After 头解析，WAF 403 修复 P0-1/P1-2）：客户端错误
// 分类在此一次完成，handler 不再对 body 二次 Classify（消除「上游分类一次、
// 网关再分类一次」的双路径漂移面），Retry-After 也随信封流动。respBody 仍原样
// 返回（错误透传语义 5755fe3：message 透传上游原文）。判定为 ErrNone 的响应
// （理论上不存在，防御）err 为 nil，handler 按 respBody 自行兜底。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// ensureConsoleSystem 在 prepareBody 后统一套用全局脚本：首条消息非 system 时前置
	// 兜底 system（防 console 域上游 code 11-128；#119 后 global 出站固定 /v2，
	// 该兜底保留——上游对 /v2 是否需要 system 无实测反证，删了无回滚路径）。
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID)
	if c.intlEndpoint(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	// reqCtx 的 cancel 在每个出口显式调用（Do 失败 / ≥400 / 成功分支移交 monitorBody），
	// 循环本身各分支必 return——无循环尾兜底代码（此前外层 var cancel 从未赋值 + 尾部
	// 不可达 cancel() 是潜伏 nil-panic，已删；chatPaths 恒非空由构造保证）。
	chatClient, cerr := c.chatHTTPFor(a)
	if cerr != nil {
		// 线路配置错误：立刻失败、**不进轮转**（换多少个号都一样），也不喂熔断 ——
		// 这是配置问题，不是账号问题（见 clientFor 的注释）。
		return nil, 0, nil, cerr
	}
	for _, path := range c.chatPaths(a) {
		url := c.chatBase(a) + path
		// 同号瞬时重试（v0.1.3 移植，默认关）：prepared 是字节切片，每次重试
		// 重建请求体（body 可重放，与轮转逻辑一致）。attempt 从 1 计数。
		attempt := 0
		for {
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
			if err != nil {
				return nil, 0, nil, err
			}
			c.ChatHeaders(req, a, clientIP, meta)
			reqCtx, cancel := context.WithCancel(ctx)
			req = req.WithContext(reqCtx)
			resp, err := chatClient.Do(req)
			if err != nil {
				cancel()
				// 传输层失败：重试开启且未耗尽 → 退避后原地重试（同一账号）。
				if c.Retry.Enabled && attempt < c.Retry.Attempts && retryAllowed(ErrNone, true) {
					attempt++
					if !sleepCtxRetry(ctx, retryBackoff(c.Retry.Backoff, attempt)) {
						return nil, 0, nil, ctx.Err() // ctx 取消：中止重试
					}
					continue
				}
				log.Printf("ERR: [upstream] chat_stream acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
				// 传输层失败 → 清空共享连接池的空闲连接（连接层加固第 5 件）：
				// 失败连接可能仍留在空闲池里，下一个请求会继续捡到它。
				roundTripCloseIdle(chatClient.Transport)
				return nil, 0, nil, err
			}
			if resp.StatusCode >= 400 {
				raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				cancel()
				// body 读失败（掐流/截断）→ 传输层错误：半截 raw 不交回调用方进 Classify。
				if rerr != nil {
					log.Printf("ERR: [upstream] chat_stream acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
					return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
				}
				kind := Classify(resp.StatusCode, string(raw))
				// 同号重试：仅 5xx（ErrServer）。429/402/401/403 等在此直接返回。
				if c.Retry.Enabled && attempt < c.Retry.Attempts && retryAllowed(kind, false) {
					attempt++
					log.Printf("WARN: [upstream] chat_stream acct=%s: retry=%d (upstream %d %s)", logfmt.Label(a.UID, a.Nickname), attempt, resp.StatusCode, kind)
					if !sleepCtxRetry(ctx, retryBackoff(c.Retry.Backoff, attempt)) {
						break // ctx 取消：中止重试，走循环尾收尾
					}
					continue
				}
				log.Printf("WARN: [upstream] chat_stream acct=%s: upstream %d %s body=%s",
					logfmt.Label(a.UID, a.Nickname), resp.StatusCode, kind, truncate(string(raw), 200))
				// 分类一次、随 Kind 信封返回（含 Retry-After 头解析，P1-2）：
				// ErrNone 是防御分支（≥400 不应产生 None），返回原文让 handler 兜底。
				if kind == ErrNone {
					return nil, resp.StatusCode, raw, nil
				}
				ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
				if d, ok := ParseRetryAfter(resp.Header); ok {
					ue.RetryAfter = d
				}
				return nil, resp.StatusCode, raw, ue
			}
			// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
			// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受。
			return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
		}
		// break 出口 = ctx 取消（重试退避期间断连）。返回 ctx 错误让 handler
		// 走既有传输错误语义（NoteFailures + 轮转）。
		if ctx.Err() != nil {
			return nil, 0, nil, ctx.Err()
		}
		// 理论不可达：retry 循环只有 return / continue / break-on-ctx 三种出口。
		return nil, 0, nil, errors.New("chat stream retry aborted")
	}
	panic("unreachable: chatPaths is never empty") // for range 空集时编译器仍要求兜底 return；chatPaths 恒非空（构造保证），永不触达
}

// chatPaths 返回按 realm 的 chat 路径候选序列：
// global → [/v2]（#119 固定单路径，见 ChatStreamContext 头注释）；cn → [/v2]（单元素，现状）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	return []string{chatCompletionsPath}
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens + 上游模型对象全字段）。
// CN /console 与 global /v2 的模型对象同构（2026-09-15 global 真实账号 /v2 探测
// 实证，字段集与任务书 hy3 样本一致），故共用此结构；上游省略的字段保持零值，
// /v1/models 侧按「空值省略」透出（不编造）。
type ModelInfo struct {
	ID             string
	Name           string
	ContextWindow  int64    // = maxInputTokens
	MaxTokens      int64    // = maxOutputTokens
	Efforts        []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort  string   // reasoning.defaultEffort（空=未声明，thinking.go 回退硬编码）
	SupportsImages bool     // 顶层 supportsImages（多模态能力，透出到 /v1/models）
	// SupportsImagesSet 上游是否**显式下发**了 supportsImages。三态语义：
	//   - Set=false → 能力未知（字段缺失）：能力预检与 hint 一律 fail-open；
	//   - Set=true, SupportsImages=false → 显式不支持（预检可拦、hint 可点名）；
	//   - Set=true, SupportsImages=true → 显式支持。
	// 为什么必须区分：`bool` 把「没下发」和「下发 false」压成同一个值，
	// 预检会把所有缺字段的模型误判成不支持（上游目录大多不带该字段）。
	SupportsImagesSet bool

	// 以下为模型目录全字段补齐（任务书 models-full-fields）：
	Description        string   // descriptionZh 中文描述
	Credits            string   // credits 积分倍率原文（如 "x0.05"），仅展示不参与选号
	Tags               []string // tags 模型标签（含 badge:限时免费 等）
	Vendor             string   // vendor 厂商标识
	IsDefault          bool     // isDefault 是否默认模型
	SupportsReasoning  bool     // supportsReasoning 是否支持推理
	SupportsToolCall   bool     // supportsToolCall 是否支持工具调用
	OnlyReasoning      bool     // onlyReasoning 是否纯推理模型
	MaxAllowedSize     int64    // maxAllowedSize 最大允许上下文（与 maxInputTokens 口径并列，上游各自下发）
	ReasoningEffort    string   // reasoning.effort 推理模式（与 supportedEfforts 数组不同源）
	ReasoningSummary   string   // reasoning.summary 推理摘要模式（如 "auto"）
	CanDisableThinking bool     // reasoning.canDisableThinking：思考可关（off 档可用）

	// 优惠（modelPromotions）：Credits 是牌价，Promo* 是当前生效优惠（面板展示用）。
	PromoFactor  *float64 // 折扣系数（0=限时免费，0.5=五折）；nil=无
	PromoCredits string   // 折扣后倍率原文（如 "0x" / "0.50x"）
	PromoLabel   string   // 徽章文案（限时免费 / 夜间折扣 / 错峰使用）
	PromoNote    string   // hover 说明原文（含时段/日期描述）
}

// dynModelEntry 上游模型目录（CN /console 与 global /v2 同构）的单条模型解析形态，
// FetchModels 与 global_models.go 的探测共用。iconUrl/descriptionEn/生成参数等
// 按「不透出」原则不解析（任务书 §不透出字段）。
type dynModelEntry struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	// SupportsImages *bool 而非 bool：nil = 上游没下发该字段（能力未知），
	// 与「显式 false」区分开（tri-state，见 ModelInfo.SupportsImagesSet）。
	SupportsImages *bool `json:"supportsImages"`
	SupportsReason bool  `json:"supportsReasoning"`
	SupportsTool   bool  `json:"supportsToolCall"`
	OnlyReasoning  bool  `json:"onlyReasoning"`
	Reasoning      struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		SupportedEfforts   []string `json:"supportedEfforts"`
		CanDisableThinking bool     `json:"canDisableThinking"`
	} `json:"reasoning"`
}

// modelInfo 按解析条目构造 ModelInfo（dynEntry→ModelInfo 映射的单一事实来源，
// CN FetchModels 与 global 探测共用，杜绝两域映射漂移）。
func (m dynModelEntry) modelInfo() ModelInfo {
	return ModelInfo{
		ID:                 m.ID,
		Name:               m.Name,
		ContextWindow:      m.MaxInputTokens,
		MaxTokens:          m.MaxOutputTokens,
		Efforts:            m.Reasoning.SupportedEfforts,
		DefaultEffort:      m.Reasoning.DefaultEffort,
		SupportsImages:     m.SupportsImages != nil && *m.SupportsImages,
		SupportsImagesSet:  m.SupportsImages != nil,
		Description:        m.Description,
		Credits:            m.Credits,
		Tags:               m.Tags,
		Vendor:             m.Vendor,
		IsDefault:          m.IsDefault,
		SupportsReasoning:  m.SupportsReason,
		SupportsToolCall:   m.SupportsTool,
		OnlyReasoning:      m.OnlyReasoning,
		MaxAllowedSize:     m.MaxAllowedSize,
		ReasoningEffort:    m.Reasoning.Effort,
		ReasoningSummary:   m.Reasoning.Summary,
		CanDisableThinking: m.Reasoning.CanDisableThinking,
	}
}

// 模型目录端点路径常量（按 realm 切）：
// CN 现状 /console/enterprises/personal/models 逐字保留（零回归）；
// global 走 /v2/enterprises/personal/models（PR #20 实测 /console 500、/v2 200 含
// credits 倍率的完整模型表）。modelsPath 按 globalOn 分发。
// v3ConfigPath 是 CN/global 双域通用的 /v3/config 模型目录端点（v3 系客户端权威
// 目录；UA 门禁：仅三段式 CLI UA 可过，CommonHeaders 即满足，见
// .claude/reports/global-models-missing.md）：v3 为主、企业端点补缺（任务书
// v3-config-merge）。
const (
	cnModelsPath     = "/console/enterprises/personal/models"
	globalModelsPath = "/v2/enterprises/personal/models"
	v3ConfigPath     = "/v3/config"
)

// modelsPath 按 realm 返回动态模型目录端点路径（不含 base）。
// CN → /console/enterprises/personal/models（现状，零回归）；
// global → /v2/enterprises/personal/models（国际版实测可用路径，见 global_models.go
// probe 家族）：governed by globalOn（config global.enabled + 账号 realm 双闸）。
func (c *Client) modelsPath(a *auth.Auth) string {
	if c.intlEndpoint(a) {
		return globalModelsPath
	}
	return cnModelsPath
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含 text-to-image：图片生成模型，非本网关用途。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		switch t {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	return false
}

// FetchModels 调上游动态模型接口（CN 侧；global 账号按 modelsPath 走 /v2，v3 补充
// 见 global_models.go FetchGlobalModels 家族，本方法职责不变）。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
//
// v3-config-merge：动态目录 = /v3/config（主）+ 企业端点（/console 或 global /v2，
// 补缺）的并集，两路**并发**探测（任务书 v3-config-merge 需求 3/4）。两域口径各自
// 保留：CN 按 agents[cli].models 过滤（零回归）；合并去重 key = 模型 id，v3 条目优先
// （credits 等字段以 v3 为准），企业端点只补 v3 缺失的模型。/v3 失败（400/网络错/解析
// 失败）不拖累企业端点结果——降级为仅企业端点，warn 日志；反之亦然（两路独立容错）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	type probeResult struct {
		infos []ModelInfo
		err   error
	}
	enterpriseCh := make(chan probeResult, 1)
	v3Ch := make(chan probeResult, 1)
	go func() {
		infos, err := c.fetchEnterpriseModels(a)
		enterpriseCh <- probeResult{infos, err}
	}()
	go func() {
		infos, err := c.fetchV3Models(a)
		v3Ch <- probeResult{infos, err}
	}()
	enterprise := <-enterpriseCh
	v3 := <-v3Ch
	if enterprise.err != nil && v3.err != nil {
		return nil, enterprise.err // 两路全失败：返回企业端点错误（既有调用方语义零漂移）
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（任务书实现要点：降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] fetch models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] fetch models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
	}
	out := mergeModelInfos(v3.infos, enterprise.infos)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入 efforts 桶）。
	// 空桶时跳过写：避免「某探测无档位数据」清掉既有桶（例：cn 桶已有档位，再次探测返回全无等级 → 不应清空）。
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	if len(cache) == 0 && len(defCache) == 0 {
		return out, nil
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.storeEfforts(a.Realm(), cache, defCache)
	return out, nil
}

// mergeModelInfos 合并两路模型目录：primary 为主（同 id 以 primary 条目为准——
// credits 等字段以主端点为权威），secondary 只补 primary 缺失的 id。
// 去重 key = 模型 id；输出顺序 = primary 原序在前、secondary 补充项（secondary 原序）
// 在后——稳定输出，不依赖 map 迭代序（任务书实现要点：排序保持稳定）。
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary)+len(secondary))
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	for _, mi := range primary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	for _, mi := range secondary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	return out
}

// fetchEnterpriseModels 单路探测企业模型端点（CN → /console/enterprises/personal/models；
// global → /v2/enterprises/personal/models，按 modelsPath 分发）。解析口径：对象形态
// agents[cli].models 过滤 + nonChatModel 剔除 + disabled 剔除（既有 FetchModels 逐字保留，
// v3-config-merge 重构抽出的单路函数）。
func (c *Client) fetchEnterpriseModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + c.modelsPath(a)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	// 按账号线路出站：这里带的是账号 Bearer，走共享 transport 就等于把流量漏到宿主 IP。
	client, cerr := c.clientFor(a, false)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误（handler 侧该路径不 NoteError，见发现 6 的正确行为）。
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	// dynMap 收集模型字段；nonChatModel 过滤在写入 dynMap 前执行，
	// 确保非对话条目（nes-/completion-/codewise- 前缀、maxOutputTokens≤256、
	// tags 含 text-to-image）根本不进返回列表（来源：harness buddy.ts:547-555）。
	dynMap := make(map[string]dynModelEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		dynMap[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, m.modelInfo())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchV3Models 单路探测 /v3/config（CN/global 双域通用，按 chatBase 切 base）。
// 解析口径与 parseGlobalModelNames 对象形态一致（data.models[].id 优先、disabled 剔除、
// 全字段落 ModelInfo）；v3 独有的 contextWindow/agent modelTags 额外字段自然忽略。
// CN 侧不按 agents[cli] 过滤（与 global 探测口径一致：v3 面取全量 models）——
// 实测 CN v3 51 模型含大量非 cli 面模型，cli 过滤后与 console 口径才有可比性；
// 但 mergeModelInfos 以 enterprise（已 cli 过滤）为 secondary 补缺，v3 全量条目中
// 只有企业端点缺失的 id 会进并集，实际生效口径仍是「cli 面并集」。
func (c *Client) fetchV3Models(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + v3ConfigPath
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// CommonHeaders 三段式 CLI UA 实测可通过 /v3/config 的 UA 门禁
	// （Bearer + web UA → 400 code 12403，见 global-models-missing.md）。
	c.CommonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	// 按账号线路出站：这里带的是账号 Bearer，走共享 transport 就等于把流量漏到宿主 IP。
	client, cerr := c.clientFor(a, false)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3 config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	names, infos, _, _, err := parseGlobalModelNames(raw)
	if err != nil {
		return nil, fmt.Errorf("v3 config parse: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("v3 config empty list")
	}
	// v3 面全量 models 不经 agents[cli] 过滤，nes-/completion-/codewise- 嵨补全/图片
	// 生成等非对话条目（企业端点的 nonChatModel 口径）同样要挡在目录外——
	// 这里按同一 nonChatModel 规则过滤（selected ID 会选模型报 code=11102）。
	out := make([]ModelInfo, 0, len(infos))
	for _, mi := range infos {
		if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
			continue
		}
		out = append(out, mi)
	}
	return out, nil
}

// billingMeterJSON 按 realm 候选路径发 billing/meter 域请求，ErrNotFound 时换下一候选路径
// （global：/billing/meter/* → /v2/billing/meter/*；cn：单路径 /v2/billing/meter/* 现状）。
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	var lastErr error
	for i, p := range paths {
		data, err := c.billingJSON(a, method, p, body)
		if err != nil {
			lastErr = err
			var ue *Error
			if i < len(paths)-1 && errors.As(err, &ue) && ue.Kind == ErrNotFound {
				continue // /billing/meter/* 404 → 换 /v2/billing/meter/*
			}
			return nil, err
		}
		return data, nil
	}
	return nil, lastErr
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	remain, _, err = c.UserResourceDetailed(a, 0)
	return remain, err
}

// CreditBuckets 按到期紧迫度拆分的积分余额（供 pool 优先消耗快过期积分）。
// 背景（issue:积分过期）：套餐/奖励积分按 CycleEndTime 分批过期，总量口径的
// remain 会让"明天就作废"的积分与"30 天后才过期"的积分被无差别选号，
// 导致快过期积分没优先用掉、白白作废。拆桶后选号可优先消耗 Expiring。
type CreditBuckets struct {
	// Expiring 在 soon 窗口内（<= now+soon）即将过期的可用积分。
	Expiring int64
	// Stable 其余有效积分（到期时间更远或无到期时间）。
	Stable int64
}

// Total 返回两桶合计可用积分（= UserResource 的 remain 口径）。
func (b CreditBuckets) Total() int64 { return b.Expiring + b.Stable }

// packageEndLayout 上游 CycleEndTime / 请求体过滤串的时间格式（墙钟）。
const packageEndLayout = "2006-01-02 15:04:05"

// expiryFloor 到期候选的**下界**（2020-01-01，UTC+8 墙钟）。
//
// 为什么需要：上游对「无到期信息」的套餐会下发零值/纪元值（`0001-01-01 00:00:00`、
// epoch 毫秒 0、`1970-01-01 00:00:00`）。这些值都能被 time 解析成功，但语义是
// 「没有这个字段」而不是「马上就要过期」——若照单全收，分桶会把整包误标成
// Expiring，让选号把长期积分当快过期积分优先烧掉（且永远不会「真的」过期）。
// 取 2020 作分界：上游业务在 2026 年才存在，任何早于此的日期都是脏值而非真实到期。
var expiryFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, softRateResetLoc)

// packageExpiryCandidate 一个套餐的到期候选值（上游同时下发多个语义不同的时间字段）。
type packageExpiryCandidate struct {
	// deductionEndMS 可抵扣窗口结束（epoch 毫秒）：真「用不完就没了」的时刻。
	deductionEndMS int64
	// expired / packageEnd / cycleEnd 墙钟字符串（packageEndLayout），空 = 未下发。
	expired, packageEnd, cycleEnd string
}

// earliestPackageEnd 返回该套餐**所有可解析候选值中最早者**，忽略零值/纪元脏值。
//
// 为什么取最早而不是按优先级取第一个：上游对同一个包同时下发
//   - CycleEndTime（周期边界，月度套餐 = 当月最后一天）
//   - DeductionEndTime（可抵扣窗口结束，实测体验版在 2034 年）
//
// 语义上「这个包的积分什么时候会消失」由更早的那个决定——只认其中一个（旧实现只认
// CycleEndTime）会在另一字段更早时低估紧迫度，把快过期积分留在 Stable 里插不上队。
//
// 全部候选都不可解析/是脏值时返回 false，调用方保守归 Stable（不误标快过期）。
func earliestPackageEnd(c packageExpiryCandidate, loc *time.Location) (time.Time, bool) {
	var (
		best time.Time
		ok   bool
	)
	// 单个候选值 → 接受条件：解析成功、不早于 expiryFloor、不早于 now 之外不管
	//（是否「已过期」由调用方按 now 判定，这里只负责挑值）。
	consider := func(t time.Time) {
		if t.Before(expiryFloor) {
			return // 零值/纪元/1970 类脏值：当作「没下发」
		}
		if !ok || t.Before(best) {
			best, ok = t, true
		}
	}
	// DeductionEndTime 是 epoch 毫秒；<=0 视为未下发（上游用 0 表示空）。
	if c.deductionEndMS > 0 {
		consider(time.UnixMilli(c.deductionEndMS).In(loc))
	}
	for _, raw := range []string{c.expired, c.packageEnd, c.cycleEnd} {
		if raw == "" {
			continue
		}
		if t, perr := time.ParseInLocation(packageEndLayout, raw, loc); perr == nil {
			consider(t)
		}
	}
	return best, ok
}

// UserResourceDetailed 同 UserResource，但按到期时间把余额拆成 CreditBuckets。
// soon>0 时把到期时间 <= now+soon 的套餐余额计入 Expiring；soon<=0 时全部归 Stable。
// 到期时间判据是 CycleEndTime（R-A/R-B 实测：CN/global 两域字段全集均无 PackageEndTime，
// 旧判据恒 miss 致 Expiring 恒 0；CycleEndTime 是上游真实下发的到期时刻——
// global Bonus Pack 14 天赠送积分的到期时间即此字段）。解析失败/缺失的套餐保守
// 归入 Stable（不误标为快过期而插队）。
// 单套餐取数统一调 packageRemainUsed（与 ResourceSummary/cmd/credit 同一事实来源，
// 含 remain 钳 [0,size] 与 used 修正；A/B 口径在 remain 维度实测一致，此改动消除
// 双份逻辑漂移——旧中间 switch 只钳负值，上游脏数据 CycleRemain>Size 时会高估）。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain int64, buckets CreditBuckets, err error) {
	now := time.Now()
	resp, err := c.getUserResourceBody(a)
	if err != nil {
		return 0, CreditBuckets{}, err
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, _, _ := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		if r < 0 {
			r = 0
		}
		remain += r
		// 分桶：仅 soon>0 且能解析出**有效**到期时间、且确实在窗口内 → Expiring。
		// 到期时间取所有候选字段中最早者（见 earliestPackageEnd）：上游同一个包
		// 可能同时下发 CycleEndTime（周期边界）与 DeductionEndTime（可抵扣窗口结束），
		// 只认其中一个会在另一字段更早时低估紧迫度。
		if soon > 0 && r > 0 {
			// 上游时间为 UTC+8 墙钟（与 softRateResetLoc 同口径，官网展示时区）。
			if end, ok := earliestPackageEnd(packageExpiryCandidate{
				deductionEndMS: acct.DeductionEndTime,
				expired:        acct.ExpiredTime,
				packageEnd:     acct.PackageEndTime,
				cycleEnd:       acct.CycleEndTime,
			}, softRateResetLoc); ok && !end.After(now.Add(soon)) {
				buckets.Expiring += r
				continue
			}
		}
		buckets.Stable += r
	}
	return remain, buckets, nil
}

// userResourceResp get-user-resource 响应结构（UserResourceDetailed 与 ResourceSummary
// 共享；含分桶所需 CycleEndTime 与聚合所需 TotalDosage，缺省字段按零值处理）。
type userResourceResp struct {
	Response struct {
		Data struct {
			TotalDosage int64 `json:"TotalDosage"`
			Accounts    []struct {
				PackageName string `json:"PackageName"`
				// 到期候选字段（分桶取所有可解析值中最早者，见 earliestPackageEnd）。
				// CycleEndTime 周期边界（墙钟字符串）；DeductionEndTime 可抵扣窗口结束
				// （epoch 毫秒，实测 CN/global 两域均下发，是体验版真失效时刻）；
				// ExpiredTime 实测字段存在但恒空；PackageEndTime 实测响应**不下发**
				//（只出现在请求体过滤参数里），保留解析以兼容未来字段。
				CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，缺省/空 = 无到期
				DeductionEndTime    int64  `json:"DeductionEndTime"`
				ExpiredTime         string `json:"ExpiredTime"`
				PackageEndTime      string `json:"PackageEndTime"`
				CapacitySize        int64  `json:"CapacitySize"`
				CapacityRemain      int64  `json:"CapacityRemain"`
				CapacityUsed        int64  `json:"CapacityUsed"`
				CycleCapacitySize   int64  `json:"CycleCapacitySize"`
				CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
				CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
			} `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// getUserResourceBody 发 get-user-resource 请求并解析响应（两消费方共享：请求体构造
// 与解析逻辑原本 100% 重复）。realm 感知继承 billingMeterPaths：global 账号打
// workbuddy.ai /billing/meter/*（404 fallback /v2），CN 账号维持
// /v2/billing/meter/get-user-resource（现状逐字，零回归）。
func (c *Client) getUserResourceBody(a *auth.Auth) (*userResourceResp, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	var resp userResourceResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}
	return &resp, nil
}

// ResourceSummary 查询账号积分套餐的完整聚合口径（remain=剩余可花积分、used=已用、
// size=总量、packs=套餐数），供运维工具（cmd/credit）按 realm 展示真实余额。
// 与 UserResource 的差异：UserResource 只取 remain；本方法额外聚合 used/size/packs，
// 且 TotalDosage 作 size 下限（与 cmd/credit 历史口径一致，见其 packageRemainUsed）。
//
// realm 感知继承 getUserResourceBody（billingMeterPaths）。
func (c *Client) ResourceSummary(a *auth.Auth) (remain, used, size int64, packs int, err error) {
	resp, err := c.getUserResourceBody(a)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, u, s := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		remain += r
		used += u
		size += s
	}
	packs = len(resp.Response.Data.Accounts)
	// TotalDosage 作 size 下限（历史口径：已消耗的不该比总剂量小）。
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := resp.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

// UserResourceTotal 返回 (剩余, 总额)。总额取 ResourceSummary 的 size（套餐总量口径），
// 供面板/导入流程展示与账面核对；请求路径请继续使用 2 返回值的 UserResource。
func (c *Client) UserResourceTotal(a *auth.Auth) (remain, total int64, err error) {
	remain, _, size, _, err := c.ResourceSummary(a)
	if err != nil {
		return 0, 0, err
	}
	return remain, size, nil
}

// respAccount 供 packageRemainUsed 解析的套餐字段（与 cmd/credit resourcePackage 同构）。
type respAccount struct {
	CapacityRemain      int64
	CapacityUsed        int64
	CapacitySize        int64
	CycleCapacityRemain int64
	CycleCapacityUsed   int64
	CycleCapacitySize   int64
}

// packageRemainUsed 聚合单套餐的 remain/used/size（历史口径见 cmd/credit/billing.go，
// 迁移至此作为单一事实来源）。Cycle 期套餐优先：用 CycleCapacity 三字段，
// used 取 CycleUsed 与 size-remain 的较大者；否则回退 Capacity 三字段。
func packageRemainUsed(a respAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return c.retryBillingTransient(func() error {
		_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
		return err
	})
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
//
// 判定顺序 inactive > already：文案同时命中两套关键词（如「签到活动已结束，今天已
// 签到」）时让位给 IsInactiveCheckin——活动结束的语义更具体，也不该被记成"今天已签"。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	if IsInactiveCheckin(err) {
		return false
	}
	return alreadyCheckinRule.hit(ue.Msg, strings.ToLower(ue.Msg))
}

// IsInactiveCheckin 报告 err 是否表示「签到活动当前不在进行中」（未开启/已结束/已过期）。
//
// 定位：这是**活动级正常态**，不是账号故障——活动下线后每天两轮签到都会收到同一条
// 业务错误。若把它记成 fail，运维会去查一个不存在的账号故障，且全灭判据会排毫无
// 意义的重试（活动没开，重试多少次都一样，纯打上游）。
//
// 因此它刻意**不是**全局 errorRule：不参与 Classify，不影响 pool 冷却/熔断。
// 调用点只有签到分支（scheduler.CheckinAll 与面板单号签到），与聊天请求的错误分类
// 完全隔离——活动状态不该让任何账号被冷却。与 IsAlreadyCheckin 一样只认 *Error：
// 网络层/解析层错误不得判 inactive（否则抖动会被当成"活动结束"，账号当天没签却显示正常）。
//
// 负向守卫：文案必须同时提到「活动」或「签到」，否则 `token 已过期` 会被误判。
func IsInactiveCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	msg := ue.Msg
	if !strings.Contains(msg, "活动") && !strings.Contains(msg, "签到") {
		return false
	}
	lower := strings.ToLower(msg)
	for _, re := range inactiveCheckinPatterns {
		if re.MatchString(msg) || re.MatchString(lower) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}

// enterpriseUnlimitedRemain / enterpriseUnlimitedTotal 企业版「不限量」在池内的等价表示
// （remain 给足够大的值让路由权重与 credit_floor 不构成限制；total=-1 表示不限量）。
const (
	enterpriseUnlimitedRemain int64 = 1 << 40
	enterpriseUnlimitedTotal  int64 = -1
)

// enterpriseResource 企业版额度查询（get-enterprise-user-usage）：
// credit 是本周期已用、limitNum 是分配额度，剩余 = limit - used（与个人口径「剩余」相反）。
func (c *Client) enterpriseResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	data, err := c.billingJSON(a, http.MethodPost, "/v2/billing/meter/get-enterprise-user-usage", map[string]any{})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	// 兼容上游两套字段命名（桌面端两条解析路径分别读 camelCase 与 snake_case）。
	var resp struct {
		Credit         float64 `json:"credit"`
		LimitNum       int64   `json:"limitNum"`
		LimitNumSnake  int64   `json:"limit_num"`
		UsedNum        float64 `json:"used_num"`
		CycleEndTime   string  `json:"cycleEndTime"`
		CycleResetTime string  `json:"cycleResetTime"`
	}
	if uerr := json.Unmarshal(data, &resp); uerr != nil {
		return 0, 0, 0, time.Time{}, 0, fmt.Errorf("enterprise resource parse: %w", uerr)
	}
	limit := resp.LimitNum
	if limit == 0 && resp.LimitNumSnake != 0 {
		limit = resp.LimitNumSnake
	}
	used := resp.Credit
	if used == 0 && resp.UsedNum != 0 {
		used = resp.UsedNum
	}
	if limit < 0 {
		return enterpriseUnlimitedRemain, enterpriseUnlimitedTotal, 0, time.Time{}, 0, nil
	}
	usedInt := int64(used)
	if used-float64(usedInt) >= 0.5 {
		usedInt++
	}
	remain = limit - usedInt
	if remain < 0 {
		remain = 0
	}
	total = limit
	now := time.Now()
	end, ok := parsePackageEndTime(resp.CycleEndTime)
	if !ok {
		end, ok = parsePackageEndTime(resp.CycleResetTime)
	}
	if ok && remain > 0 && end.After(now) {
		earliestAt, earliestRemaining = end, remain
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring = remain
		}
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// UserResourceDetailedWithExpiry 返回 (剩余, 总额, 窗口内快过期, 最早到期时刻, 最早批次剩余)。
// 企业版走 enterpriseResource 分流；个人版按套餐 CycleEndTime 聚合（与面板到期分布同口径）。
func (c *Client) UserResourceDetailedWithExpiry(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	if a.IsEnterprise() {
		return c.enterpriseResource(a, soon)
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	var data json.RawMessage
	if rerr := c.retryBillingTransient(func() error {
		var e error
		data, e = c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
		return e
	}); rerr != nil {
		return 0, 0, 0, time.Time{}, 0, rerr
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CycleEndTime        string `json:"CycleEndTime"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, time.Time{}, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, _, size := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		if r <= 0 {
			continue
		}
		end, ok := parsePackageEndTime(acct.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
			earliestRemaining = r
		} else if end.Equal(earliestAt) {
			earliestRemaining += r
		}
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += r
		}
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// CreditPackage 单个积分包（面板「积分构成/到期分布」用）。
type CreditPackage struct {
	Name           string `json:"name"`
	Remain         int64  `json:"remain"`
	Used           int64  `json:"used"`
	Size           int64  `json:"size"`
	EndTime        string `json:"end_time,omitempty"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	PackageCode    string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	Cycle          bool   `json:"cycle,omitempty"`
}

// CreditPackages 拉取账号积分包清单，返回 (包列表, 剩余合计, 面额合计)。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CapacitySize        int64  `json:"CapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					ExpiredTime         string `json:"ExpiredTime"`
					PackageEndTime      string `json:"PackageEndTime"`
					CycleEndTime        string `json:"CycleEndTime"`
					DeductionEndTime    int64  `json:"DeductionEndTime"`
					CreateTime          int64  `json:"CreateTime"`
					PackageCode         string `json:"PackageCode"`
					SubProductCode      string `json:"SubProductCode"`
					SubProductName      string `json:"SubProductName"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}
	packs := resp.Response.Data.Accounts
	out := make([]CreditPackage, 0, len(packs))
	var sumRemain, sumSize int64
	for _, p := range packs {
		cp := CreditPackage{
			Name:           p.PackageName,
			PackageCode:    p.PackageCode,
			SubProductCode: p.SubProductCode,
			SubProductName: p.SubProductName,
		}
		switch {
		case p.DeductionEndTime > 0:
			cp.EndTime = time.UnixMilli(p.DeductionEndTime).Format(time.RFC3339)
			cp.ExpiresAt = p.DeductionEndTime
		case p.ExpiredTime != "":
			cp.EndTime = p.ExpiredTime
		case p.PackageEndTime != "":
			cp.EndTime = p.PackageEndTime
		default:
			cp.EndTime = p.CycleEndTime
		}
		if cp.ExpiresAt == 0 && cp.EndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, cp.EndTime, softRateResetLoc); perr == nil {
				cp.ExpiresAt = end.UnixMilli()
			}
		}
		if p.CreateTime > 0 {
			cp.CreatedAt = time.UnixMilli(p.CreateTime).Format(time.RFC3339)
		}
		if p.CycleCapacitySize > 0 {
			cp.Cycle = true
			cp.Remain, cp.Size = p.CycleCapacityRemain, p.CycleCapacitySize
			cp.Used = cp.Size - cp.Remain
			if p.CycleCapacityUsed > cp.Used {
				cp.Used = p.CycleCapacityUsed
				cp.Remain = cp.Size - cp.Used
			}
		} else {
			cp.Remain, cp.Size, cp.Used = p.CapacityRemain, p.CapacitySize, p.CapacityUsed
			if cp.Used == 0 && cp.Size > cp.Remain {
				cp.Used = cp.Size - cp.Remain
			}
		}
		if cp.Remain < 0 {
			cp.Remain = 0
		}
		if cp.Size > 0 && cp.Remain > cp.Size {
			cp.Remain = cp.Size
		}
		sumRemain += cp.Remain
		sumSize += cp.Size
		out = append(out, cp)
	}
	return out, sumRemain, sumSize, nil
}

// ModelRate 返回某 realm 下模型的生效倍率原文（panel 模型视图用；无缓存返回空串）。
func (c *Client) ModelRate(realm, model string) string {
	if c == nil || model == "" {
		return ""
	}
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	return c.modelRates[realmKey(realm)][model]
}

// storeModelRates 按 realm 整体替换模型倍率快照（FetchModels 刷新后调用）。
func (c *Client) storeModelRates(realm string, infos []ModelInfo) {
	rates := make(map[string]string, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		if rate := effectiveModelRate(mi); rate != "" {
			rates[mi.ID] = rate
		}
	}
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.modelRates == nil {
		c.modelRates = make(map[string]map[string]string)
	}
	c.modelRates[realmKey(realm)] = rates
}

// effectiveModelRate 生效价：有促销且给出折扣倍率原文时用促销价，否则牌价。
func effectiveModelRate(mi ModelInfo) string {
	if mi.PromoFactor != nil && strings.TrimSpace(mi.PromoCredits) != "" {
		return normalizeModelRate(mi.PromoCredits)
	}
	return normalizeModelRate(mi.Credits)
}

// normalizeModelRate 归一 fang 倍率原文（"x0.05" → "0.05"；"0x" → "0"）。
func normalizeModelRate(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(s), "credits") {
		s = strings.TrimSpace(s[:len(s)-len("credits")])
	}
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "x") {
		s = strings.TrimSpace(s[1:])
	} else if strings.HasSuffix(low, "x") {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	return s
}

// parsePackageEndTime 解析上游套餐到期墙钟（UTC+8，格式见 packageEndLayout）。
func parsePackageEndTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(packageEndLayout, raw, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// SetCABundle 在系统根证书之外追加 PEM 根证书（企业代理/Clash TLS 解密场景），
// 不关闭证书校验；空路径 = 无操作。文件不可读或没有证书时返回错误（启动期 fail fast）。
func (c *Client) SetCABundle(path string) error {
	pem, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read ca bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("no certificates found in %s", path)
	}
	tr := newTransport()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	c.HTTP.Transport = tr
	if c.ChatHTTP != nil {
		c.ChatHTTP.Transport = tr
	}
	// 线路 transport 也从模板克隆 —— 否则走 TLS 解密代理时它们仍用系统根证书，
	// ca_bundle 对绑了线路的账号完全无效。
	ConfigureBaseTransport(tr)
	return nil
}
