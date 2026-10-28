package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

type ChatExecOptions struct {
	Body      []byte
	Stream    bool
	Protocol  string
	OnError   func(status int, code, msg, hint string)
	OnAccount func(*auth.Auth)
	Stat      *chatStat
	// RequestProfile 出站兼容 profile 覆盖（空 = 用 livecfg 的 upstream.request_profile）。
	RequestProfile upstream.RequestProfile
}

type ChatExecution struct {
	RC             io.ReadCloser
	Account        *auth.Auth
	Release        func()
	Status         int
	Aggregated     map[string]any
	BareModel      string
	Realm          string
	ModelRate      string
	ChatMeta       upstream.ChatMeta
	Stat           *chatStat
	AttemptStarted time.Time
}

// chatPeek 是请求体里与选号/协议分流相关的少量字段。
type chatPeek struct {
	Stream bool   `json:"stream"`
	Model  string `json:"model"`
}

func peekChatBody(body []byte) (chatPeek, error) {
	var p chatPeek
	err := json.Unmarshal(body, &p)
	return p, err
}

// preparedChat 是预检后的协议无关请求上下文。
type preparedChat struct {
	Body       []byte
	Stream     bool
	Protocol   string
	OnError    func(status int, code, msg, hint string)
	OnAccount  func(*auth.Auth)
	Peek       chatPeek
	Realm      string
	BareModel  string
	ModelRate  string
	Profile    string
	CrossRealm bool
	// ChatMeta / StickyKey / ReqHasImage / KeyGroups 由 executeChat（Task 4）
	// 在轮转段填充；prepareChatRequest 不设置它们。
	ChatMeta       upstream.ChatMeta
	StickyKey      string
	DeclaredTools  map[string]bool
	ReqHasImage    bool
	KeyGroups      []string
	Stat           *chatStat
	RequestProfile upstream.RequestProfile
}

// ChatExecError 是预检阶段产生的协议无关错误。
type ChatExecError struct {
	Status     int
	Code       string
	Msg        string
	Hint       string
	ImageCount int
	MaxImages  int
}

func (e *ChatExecError) Error() string { return e.Code + ": " + e.Msg }

// readBody 是各协议入口共用的请求体读取（含 server.max_body_mb 上限）。
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if h.cfg.MaxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	}
	return io.ReadAll(r.Body)
}

// prepareChatRequest 执行读 body 之后的请求预检：模型/realm 解析、别名、
// 密钥白名单与 profile、图片策略/能力预检、models.hidden。成功时返回
// preparedChat，失败时返回协议无关的 ChatExecError。
func (h *Handler) prepareChatRequest(r *http.Request, opt ChatExecOptions) (*preparedChat, *ChatExecError) {
	body := opt.Body
	peek, err := peekChatBody(body)
	if err != nil {
		// 请求体必须是合法 JSON 对象：否则不该进入选号/轮转。旧行为是把坏 body 逐个
		// 账号转发给上游（实测 reqlog attempts=2），白耗配额；上游对重复非法请求会回
		// 403 account_fault(11140)，网关再按封禁禁用 → 客户端一个坏请求就能误禁健康号
		// （实测）。本地直接 400，零上游请求、不罚账号。
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "invalid_request",
			Msg:    "request body is not valid JSON: " + err.Error(),
		}
	}
	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
	// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」
	// 与「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := upstream.ToolNameAllowlist(body)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/账本/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel, realmExplicit := resolveModelDetail(peek.Model)
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}
	// 模型别名/后台任务重定向（upstream.model_aliases，默认空 = 不启用）：桌面客户端
	// 后台任务会硬编码付费模型，可在此改写到免费目标（省积分）。命中记日志；realm
	// 前缀保持，选号/账本/粘性都用改写后的目标模型。
	if target := matchModelAlias(h.cfg.ModelAliases, bareModel); target != "" && target != bareModel {
		log.Printf("alias: model %s -> %s", bareModel, target)
		bareModel = target
	}
	// 跨域回退（issue #135）：只有裸模型名 + 显式开启 pool.cross_domain_fallback 时生效。
	// 语义 = 「裸名不绑定单一域」：首选域（裸名默认 cn）选不到可用号（账号级冷却/熔断/
	// 在途占满，或该域账号对这个模型 11102 后进入模型级负缓存）时，改用另一域再选一次；
	// 显式 cn:/global: 前缀保持原语义，永不回退。
	crossRealm := false
	if h.cfg.Pool != nil {
		crossRealm = !realmExplicit && h.cfg.Pool.CrossDomainFallback()
	}

	// 密钥级模型白名单 + realm 绑定（吸收自 hub）：不匹配本地 400，零上游请求。
	key := authKeyFrom(r.Context())
	if key != nil {
		if key.Realm != "" && key.Realm != realm {
			return nil, &ChatExecError{
				Status: http.StatusBadRequest,
				Code:   "key_realm_mismatch",
				Msg:    "this api key is bound to realm " + key.Realm + ", but the requested model routes to " + realm,
			}
		}
		if len(key.Models) > 0 && !matchModelPatterns(key.Models, bareModel) {
			return nil, &ChatExecError{
				Status: http.StatusBadRequest,
				Code:   "model_not_allowed",
				Msg:    "model " + bareModel + " is not allowed for this api key",
			}
		}
	}
	// 产品 profile（契约 §1 / Todo 22）：越界/未知 header 在选号前 400；
	// profiles 未激活时恒为空，选号行为与迁移前逐字一致。
	profile, perr := h.resolveRequestProfile(r, key)
	if perr != nil {
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   perr.Code,
			Msg:    perr.Msg,
		}
	}
	if profile != "" && auth.ProfileRealm(profile) != realm {
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "key_realm_mismatch",
			Msg: "profile " + profile + " routes to realm " + auth.ProfileRealm(profile) +
				", but the requested model routes to " + realm,
		}
	}
	if profile != "" {
		// 目录冷时异步补一次，供后续请求的能力预检使用；本次不阻塞。
		go h.warmProfileModels(profile)
	}

	// 图片策略与能力预检（Todo 12）：**读 body 之后、选号之前**。
	// 位置理由：两者都只依赖请求体 + 模型目录（不依赖账号），放在选号前意味着
	// 被拒请求零上游 chat 请求、不占租约、不罚任何账号。
	// 开关一律从 livecfg 取（面板热改），零值 = 迁移前行为（0/passthrough/false）。
	live := h.loadLive()
	body, imageCount, imgErr := upstream.ApplyImagePolicy(body, live.MaxImages, live.ImagePolicy)
	if imgErr != nil {
		var limitErr *upstream.ImagePolicyError
		if errors.As(imgErr, &limitErr) {
			return nil, &ChatExecError{
				Status: http.StatusRequestEntityTooLarge,
				Code:   "too_many_images",
				Msg: fmt.Sprintf("request contains %d images, which exceeds upstream.max_images=%d; reduce the number of images in messages",
					limitErr.ImageCount, limitErr.MaxImages),
				ImageCount: limitErr.ImageCount,
				MaxImages:  limitErr.MaxImages,
			}
		}
		// 其他错误（当前不可达：ApplyImagePolicy 只在超限且 policy=error 时报错）
		// 不猜语义，按参数错误就地拒绝。
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "invalid_request",
			Msg:    imgErr.Error(),
		}
	}
	// 只在真的丢掉图片时打一行：策略开启但请求未超限是常态，不该刷日志。
	// 不二次 CountImages（body 已改写，再解析一次只为日志不值当）。
	if live.ImagePolicy == "truncate" && imageCount > live.MaxImages {
		log.Printf("[server] image policy: truncated %d -> %d (upstream.max_images=%d)", imageCount, live.MaxImages, live.MaxImages)
	}
	// 能力预检：仅当开关开、请求确实带图、且目录**显式**声明不支持（tri-state
	// Set=true + false）时拒绝。能力缺失（Set=false）一律 fail-open——上游目录
	// 大多数条目不下发 supportsImages，按「缺失 = 不支持」拦会把正常请求全挡掉。
	if live.CapabilityGuard && hasImagePart(body) {
		if cap := h.lookupModelCapability(realm, bareModel, profile); cap.InCatalog && cap.SupportsImagesSet && !cap.SupportsImages {
			return nil, &ChatExecError{
				Status: http.StatusBadRequest,
				Code:   "model_capability_mismatch",
				Msg:    "model " + bareModel + " does not support images",
				Hint:   upstream.NoImagesHint(bareModel),
			}
		}
	}
	// models.hidden 黑名单：点名调用直接拒绝，不打上游、不占选号。
	// 与 /v1/models 的列表过滤对称（只藏列表不够，知道名字仍可硬调）。
	if _, hide := h.loadLive().HiddenModels[realm+":"+bareModel]; hide {
		return nil, &ChatExecError{
			Status: http.StatusBadRequest,
			Code:   "model_unavailable",
			Msg:    "model is hidden by gateway configuration, try another model",
		}
	}

	requestProfile := upstream.RequestProfile(h.loadLive().RequestProfile)
	if opt.RequestProfile != "" {
		requestProfile = opt.RequestProfile
	}
	return &preparedChat{
		Body:           body,
		Stream:         opt.Stream,
		Protocol:       opt.Protocol,
		OnError:        opt.OnError,
		OnAccount:      opt.OnAccount,
		Peek:           peek,
		Realm:          realm,
		BareModel:      bareModel,
		ModelRate:      modelRate,
		Profile:        profile,
		RequestProfile: requestProfile,
		CrossRealm:     crossRealm,
		DeclaredTools:  declaredTools,
		Stat:           opt.Stat,
	}, nil
}

// writeChatError 把协议无关错误交给调用方注入的错误写入器。
func (h *Handler) writeChatError(onError func(int, string, string, string), status int, code, msg, hint string) {
	if onError != nil {
		onError(status, code, msg, hint)
	}
}

// chatKeyName 返回本次请求的密钥用量归因名：主密钥 = "main"，
// api_keys 条目用其 Name（未命名条目回落 "unnamed"）。
func chatKeyName(r *http.Request) string {
	keyName := "main"
	if ki := authKeyFrom(r.Context()); ki != nil {
		if ki.Name != "" {
			keyName = ki.Name
		} else {
			keyName = "unnamed"
		}
	}
	return keyName
}

// recordChatAttempt 记录一次账号尝试：请求统计、账号用量台账与用量时序。
func (h *Handler) recordChatAttempt(st *chatStat, keyName, bareModel, fallbackModel, modelRate, uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
	st.attempts++
	if delta.HasPromptTokens {
		st.prompt = int(delta.PromptTokens)
	}
	if delta.HasCompletionTokens {
		st.toks = int(delta.CompletionTokens)
	}
	st.hasUsage = delta.HasPromptTokens || delta.HasCompletionTokens || delta.HasTotalTokens
	delta.Model = bareModel
	if delta.Model == "" {
		delta.Model = fallbackModel
	}
	latency := time.Since(started)
	latencyMs := latency.Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}
	delta.HasLatencyMs = true
	delta.LatencyMs = latencyMs

	// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
	// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
	if delta.HasCompletionTokens {
		if tps, ok := tokensPerSecond(delta.CompletionTokens,
			time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = tps
		}
	}
	h.cfg.Pool.RecordTokenUsage(uid, delta)

	// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
	// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
	// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
	if h.cfg.Usage != nil {
		realm := "cn"
		if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
			realm = a.Realm
		}
		h.cfg.Usage.AddKeyed(time.Now(), realm, uid, delta.Model, keyName, usage.Delta{
			PromptTokens:     delta.PromptTokens,
			HasPromptTokens:  delta.HasPromptTokens,
			CompletionTokens: delta.CompletionTokens,
			HasCompletion:    delta.HasCompletionTokens,
			TotalTokens:      delta.TotalTokens,
			HasTotal:         delta.HasTotalTokens,
			Credit:           credit,
			HasCredit:        hasCredit,
			HasCacheTokens:   st.hasCache,
			CacheHitTokens:   int64(st.cacheHit),
			CacheMissTokens:  int64(st.cacheMiss),
			LatencyMs:        delta.LatencyMs,
			HasLatency:       delta.HasLatencyMs,
			TokensPerSecond:  delta.TokensPerSecond,
			HasTPS:           delta.HasTokensPerSecond,
			ModelRate:        modelRate,
		}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
	}
}

// executeChat 执行选号、刷新、轮转与错误处置。成功时返回上游流或聚合体，
// 由调用方负责协议相关的响应写出；所有错误经 prep.OnError 输出。
func (h *Handler) executeChat(ctx context.Context, r *http.Request, prep *preparedChat) (*ChatExecution, bool) {
	st := prep.Stat
	if st == nil {
		st = newChatStat(time.Now(), prep.Body, prep.Stream, h.budget)
		prep.Stat = st
	}
	body := prep.Body
	peek := prep.Peek
	realm := prep.Realm
	bareModel := prep.BareModel
	modelRate := prep.ModelRate
	crossRealm := prep.CrossRealm
	profile := prep.Profile
	key := authKeyFrom(r.Context())
	tried := map[string]bool{}
	// 账号日限额（pool.account_daily_credit_limit，0 = 关闭）：当日 credit 已达上限的
	// 账号本轮不参与选号；全部触顶时末端回 429 daily_limit_exceeded。
	var dailyLimited bool
	blockedByDaily := 0
	if h.cfg.AccountDailyCreditLimit > 0 && h.cfg.Usage != nil {
		used := h.cfg.Usage.AccountCredit(usage.Window{From: cstDayStart(time.Now())})
		for uid, c := range used {
			if c >= h.cfg.AccountDailyCreditLimit {
				tried[uid] = true
				blockedByDaily++
			}
		}
	}
	var lastErr error
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// 按模型解析：同一个会话可能换模型，绑定号若在当前模型上被 6004 限额（对其他模型
	// 仍可用），必须重分配——否则会被钉在这个号上反复失败。
	// 提取与下方会话头族的聚合键共用同一结果，故**不受粘性开关影响**：粘性未启用
	// （Session==nil）时聚合键仍应是会话级，而不是退化成轮级。
	sessKey := session.ExtractKey(body)
	// stickyKey 是**粘性专用**键，与 sessKey（会话头族聚合用）分开：
	// sessKey 为空时（OpenAI 兼容客户端——dsh / Codex 等既无 conversationId 也无
	// metadata）用首条 user 消息派生会话级 fallback 键，使粘性仍能生效。
	// 不能直接改 sessKey：那会连带改变上游头族轮级复合键（sessKey 入键）的聚合
	// 语义，属于另一条链路的契约。
	stickyKey := sessKey
	if stickyKey == "" {
		stickyKey = session.StickyFallbackKey(body)
	}
	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		// 传给 ResolveForModel 的是**完整**模型名（peek.Model，含 realm 前缀）。
		// 粘性命中校验走 injected AvailableForModel 闭包 → 闭包内部 resolveModel 剥前缀
		// 得 realm+bare，再按 realm 过滤可用集合。若传已剥前缀的 bareModel，闭包对裸名
		// 恒剥出 realm=cn，跨 realm 粘性会话会被错误钉回 CN 集合；完整前缀才能让
		// 闭包正确过滤到 global 集合（见 cmd/server/wiring.go realmAwareAvailableForModel）。
		// 模型名也参与成本账本与选号过滤，不能用 "-" 占位污染模型键。
		resolve := h.cfg.Session.ResolveForModel
		if h.cfg.Pool.StrategyForRealm(realm) != pool.WeightedRandom {
			resolve = h.cfg.Session.LookupForModel
		}
		if uid, ok := resolve(stickyKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（与官方桌面 CLI 的
	// X-Conversation-Request-ID 轮级语义对齐），故不再限 sessKey=="" 才计算；
	// sessKey 由调用侧以复合键方式入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite / rewriteModel 之前取——改写会动 messages 内容。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	keepLease := false
	defer func() {
		if !keepLease && heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(stickyKey)
			stickyUID = ""
		}
	}
	// 粘性号若已触达当日限额（tried 预置），解绑避免钉死。
	if stickyUID != "" && tried[stickyUID] {
		unbindSticky()
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 本次请求的密钥身份（多 Key 用量归因）：主密钥 = "main"，api_keys 条目用其
	// Name（未命名条目回落 "unnamed"）。
	keyName := "main"
	if ki := authKeyFrom(r.Context()); ki != nil {
		if ki.Name != "" {
			keyName = ki.Name
		} else {
			keyName = "unnamed"
		}
	}

	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		h.recordChatAttempt(st, keyName, bareModel, peek.Model, modelRate, uid, delta, credit, hasCredit, started, ttfb)
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35 / #170）：后台按 X-Conversation-Request-ID 聚合请求，官方
	// 客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。**统一轮级**
	// （对齐官方桌面 CLI：TraceStartHook 每次 USER_PROMPT_SUBMIT 清空重生成
	// conversationRequestId，同轮内复用、跨轮必换；官方云链路 lfConvReqId 的会话级
	// 是服务端指令，走本网关的客户端不属于该形态）。此处**轮转循环外**生成一次，
	// 循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再碎片化（此前网关
	// 一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个 RequestID）。
	//   - conversationID：body 提取（透传客户端原值）；缺省时在下方用轮级聚合键
	//     ConversationHeaderID 兜底出站 X-Conversation-ID（上游渠道校验要求该头恒在；
	//     兜底值只进出站头、不进 prompt_cache_key 会话段，缓存键仍按客户端原值——
	//     轮级键逐轮变化不会打碎前缀缓存）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先（客户端已
	//     有自己的对话轮 ID 则以客户端为准）。派生分两态：
	//     * turnKey 非空（有末条 user 消息）→ 带会话键客户端走 TurnRequestID(
	//       sessKey+":"+turnKey) 复合键（会话段入键保证不同会话同轮文本不互撞，
	//       轮级粒度对齐官方 CLI）；无会话键客户端走既有 TurnRequestID(turnKey)
	//       纯轮级键（存量会话键值零漂移）。
	//     * turnKey 为空（残留空态：无 user 消息/无可签名内容）→ sessKey 非空时
	//       回落 RequestIDForKey(sessKey)（会话级兜底，好于请求级随机）；sessKey
	//       也空走 NewMessageID 请求级（TurnRequestID 空键行为）——轮转内捕获
	//       一次即共享。
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body), RequestProfile: prep.RequestProfile}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底：无轮可聚合时维持会话级聚合（同会话恒同值）。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	// 上游渠道校验要求出站 X-Conversation-ID 恒在（逐账号直连实测：只要头在场即通过，
	// 不校验会话存在性；缺头则返回 400 code=11-128 "conversation id missing"）。客户端
	// 没带 conversationId 时，用上面已算好的轮级聚合键兜底——同轮内重试/换号/降级复用
	// 同一会话 ID，与 ConversationRequestID 的聚合语义对齐（#170 统一轮级）。
	// 只写 ConversationHeaderID 而不覆盖 ConversationID：后者仍是 prompt_cache_key
	// 会话段的哈希源，保持无会话键客户端的「每账号恒键」前缀缓存命中率不被打碎。
	if chatMeta.ConversationID == "" {
		chatMeta.ConversationHeaderID = chatMeta.ConversationRequestID
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// 本次请求密钥的**可见业务分组**（nil = 不限分组：主密钥 / 未配置 api_keys）。
	// 鉴权已在 withAuth 完成并把命中的密钥条目注入 context；此处只取值。
	// ★ 选号链路的每一处都必须带上它 ★（普通轮换、全冷却兜底、粘性命中），
	// 漏掉任何一处就是一个隔离缺口，而且缺口只在特定时序下才暴露。
	var keyGroups []string
	if key != nil {
		keyGroups = key.Groups
	}

	prep.ChatMeta = chatMeta
	prep.StickyKey = stickyKey
	prep.ReqHasImage = reqHasImage
	prep.KeyGroups = keyGroups

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			// 粘性命中同样受分组约束：绑定号若属于其他密钥的分组，返回 nil → 解绑重分配。
			acct = h.cfg.Pool.PickByUIDForModelGroupsProfiles(stickyUID, bareModel, keyGroups, profile)
			if acct == nil || (realm != "" && acct.Realm() != realm && !crossRealm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）、realm 不符，
				// 或不在本次密钥的可见分组内 → 解绑。
				unbindSticky()
			}
		}
		if acct == nil {
			pickRealm := realm
			if crossRealm && h.cfg.Pool.ServableForModelRealm(bareModel, realm) == 0 &&
				h.cfg.Pool.ServableForModelRealm(bareModel, otherRealm(realm)) > 0 {
				pickRealm = otherRealm(realm)
			}
			acct = h.cfg.Pool.PickExcludingForRealmGroupsProfiles(tried, bareModel, pickRealm, keyGroups, profile)
			if acct == nil && crossRealm {
				acct = h.cfg.Pool.PickExcludingForRealmGroupsProfiles(tried, bareModel, otherRealm(pickRealm), keyGroups, profile)
			}
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			if blockedByDaily > 0 {
				dailyLimited = true
			}
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分（issue #102 附带发现 1）。
			modelBlock = h.cfg.Pool.ModelBlocked(bareModel)
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		if crossRealm && h.cfg.Upstream != nil && acct.Realm() != realm {
			// 跨域回退命中：按实际服务域取积分倍率，用量/账本记的是真实域口径，
			// 而不是请求前缀声明的域。
			modelRate = h.cfg.Upstream.ModelRate(acct.Realm(), bareModel)
		}
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次粘性命中往返（语义与 fail()/粘性命中-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, ctx) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				h.cfg.Pool.NoteRefreshFail(acct.UID, err) // P0-1 续期观测台账（只记录，不判罚）
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 12153 一次失败不杀号（临时触发会误杀）：与 scheduler keepalive/checkin
					// 同口径走连续计数，达到 sessionDeadThreshold 才禁用。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, ctx) {
					break // ctx 取消：终止轮转（refresh 失败换号退避，WAF P0-2）
				}
				continue
			}
			h.cfg.Pool.NoteRefreshOK(acct.UID) // P0-1：续期成功，留证（与失败分支对称）
			acct.BackfillRealm()               // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：按请求取首段作为参数传入 ChatStream，
		// 不再读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		// 传 ctx：客户端断连/请求取消立即中断在途上游调用并释放租约，
		// 不再让"幽灵请求"占满账号在途名额直到 IdleTimeout。
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(ctx, acct, body, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析，见 ChatStreamContext 注释）。传输层错误（非 *Error）走
		// 抖动换号分支；防御分支（terr 为 nil 但 status>=400，如 ErrNone 兜底）回落
		// 本地 Classify，双保险不改变语义。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		// 线路配置错误（账号绑定的线路名不存在 / 地址非法 / 上游没配任何线路）：
		// 这是**配置问题，不是账号问题** —— 换多少个号结果都一样。此前它落进下面的
		// 「网络抖动」分支，会 NoteFailures + fail + 换号，一次拼错就能把整池打空
		// 并最终 503，而且日志里只看到「no healthy account」。这里直接终止轮转。
		if uerr != nil && uerr.Kind == upstream.ErrRouteConfig {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = uerr.Status
			lastErr = terr
			log.Printf("ERROR: [server] route config acct=%s: %v (rotation stopped, account not penalized)",
				logfmt.Label(acct.UID, acct.Nickname), terr)
			break
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：**不换号、不罚号**。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，
			// 只会把客户端拖到 MaxRotate × header_timeout（部署值 600s 时最坏
			// 约半小时），期间还给一串健康号喂连败计数。此前全仓没有任何超时
			// 识别，超时和"网络抖动"共用同一条换号路径。
			//
			// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、
			// 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）、以及
			// **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流，
			// 也就是上游停滞。客户端主动断连时 r.Context() 已取消，走下面的抖动分支。
			if isUpstreamTimeout(terr, ctx.Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				log.Printf("WARN: [server] upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
					logfmt.Label(acct.UID, acct.Nickname), terr)
				break
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, ctx) {
				break // ctx 取消：终止轮转（传输层错误换号退避，WAF P0-2）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样，
				// 任务书授权上游错误码/账号语义对客户端可见），不再改写成网关固定文案。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				h.writeChatError(prep.OnError, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), realm, bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return nil, false
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作（不冷却/不熔断/不 NoteError，
			// 不喂连败），fail 只释放租约。error-passthrough：message 装上游 body 原文
			// （code/msg/requestId 原样，含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				h.writeChatError(prep.OnError, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), realm, bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return nil, false
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				h.writeChatError(prep.OnError, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), realm, bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return nil, false
			}
			// 6021「queue.full.title」渠道级容量满载：**不罚号不轮转**，立即透传
			// 上游原文回客户端（503 + upstream_busy）。上游集群排队打满时换任何
			// 账号都撞同一堵墙——按限流处置会逐号软冷却，实测 2 秒绞杀整池健康号
			// （2026-10-15 实案），且换号白撞把无效请求放大 MaxRotate 倍。客户端
			// 按 SDK 退避重试即可；等待窗口过去渠道恢复即成功。
			if kind == upstream.ErrUpstreamBusy {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "upstream capacity is full (queue.full); retry after a short wait"
				}
				h.writeChatError(prep.OnError, http.StatusServiceUnavailable, "upstream_busy", msg,
					h.hintOf(upstream.ErrUpstreamBusy, string(respBody), realm, bareModel, reqHasImage, uerr))
				st.status = http.StatusServiceUnavailable
				return nil, false
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 5755fe3 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义，
			// 任务书设计纪律）：该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，
			// IP 被拦而非账号）则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍
			// 打同一出口 IP，加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账
			// （单号偶发 403 仍冷却），IP 级状态只改变「是否继续轮转」——协同不叠加。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, ctx) {
				break // ctx 取消：终止轮转（分类错误换号退避，WAF P0-2）
			}
			continue
		}
		if prep.OnAccount != nil {
			prep.OnAccount(acct)
		}
		// 成功判定与粘性绑定一律**延后到调用方真正成功写出之后**：
		// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
		// 由调用方在流尾分类处置。这里只把上游流/聚合体交出去。
		if prep.Stream {
			keepLease = true
			return &ChatExecution{
				RC: rc, Account: acct, Release: releaseHeld, Status: http.StatusOK,
				BareModel: bareModel, Realm: realm, ModelRate: modelRate,
				ChatMeta: prep.ChatMeta, Stat: st, AttemptStarted: attemptStarted,
			}, true
		}
		raw, aerr := upstream.Aggregate(rc)
		_ = rc.Close()
		if aerr != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			h.writeChatError(prep.OnError, http.StatusBadGateway, "upstream_parse", aerr.Error(), "")
			st.status = http.StatusBadGateway
			return nil, false
		}
		keepLease = true
		return &ChatExecution{
			Aggregated: raw, Account: acct, Release: releaseHeld, Status: http.StatusOK,
			BareModel: bareModel, Realm: realm, ModelRate: modelRate,
			ChatMeta: prep.ChatMeta, Stat: st, AttemptStarted: attemptStarted,
		}, true
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	//
	// 背景：此前把上游原始错误文本统一改写，防账号 UID / 上游内部错误码（11128 / 12153 /
	// 6004 后台措辞）泄露——但副作用是客户端看不到真实错误，根本没法排查上游问题。
	// 任务书规定上游错误码/账号语义**允许**泄露给客户端（有意为之），排查必须看到原文。
	//
	//   - 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	//     requestId 原样保留，如 {"code":6004,"msg":"…","requestId":"…"}）。HTTP 状态码
	//     按 OpenAI 兼容口径映射类别：ErrSoftRate → 429（限流语义、客户端应等待重试），
	//     其余保持 503（网关侧无健康账号可用）。业务 code 取本地分类可读名
	//     （rate_limit_exceeded / no_healthy_account）。
	//   - 本地调度类错误（无可用账号 acct==nil、传输层抖动、非上游返回的 lastErr）
	//     → 保留自有文案 no_healthy_account（本地错误没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进"没有可用账号"——两者的排查方向完全不同。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	if lastErr == nil && dailyLimited {
		status = http.StatusTooManyRequests
		code = "daily_limit_exceeded"
		msg = "all accounts reached their daily credit limit (pool.account_daily_credit_limit); retry after the CST daily reset"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定（11133/11135
	// 在 hint 层自带形态判定，ErrClient 家族也能带上 hint）；本地调度类错误
	// （无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, realm, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：空 body 时给出明确可读文案——
				// 网关出口 IP 被 WAF 拦截、轮转已止损、窗口 X 秒后自动解除。客户端
				// 提前重试无意义（换号不换 IP）；有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrUpstreamBusy:
			// 6021 queue.full 渠道级容量满载（防御路径：正常已在轮转循环内
			// fail-fast 拦截）。503 + 可重试语义——上游原文（下方统一透传）
			// 含 requestId，客户端按 SDK 退避重试即可。
			code = "upstream_busy"
			msg = "upstream capacity is full (queue.full); retry after a short wait"
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试，而真正该做的是换个模型。用 400 + 明确的 code
			// 把不可重试的性质讲清楚。
			code = "model_unavailable"
			status = http.StatusBadRequest
		case upstream.ErrModelEntitlement:
			// 11158「model not allowed for user type」：模型级权限（免费号不可用），
			// 不是账号问题——400 + 明确 code，客户端应换模型而非重试。
			status = http.StatusBadRequest
			code = "model_not_allowed"
			msg = "this model is not available for the current account type (11158), try another model"
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（放在最后 = 优先级最高）。
	//
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	if modelBlock.Blocked {
		code = "model_unavailable"
		// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
		// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
		if !upstreamMsgPassed {
			msg = "model is unavailable on every account (per-model cooldown), try another model"
		}
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
		status = http.StatusBadRequest
	}
	h.writeChatError(prep.OnError, status, code, msg, hint)
	st.status = status
	return nil, false
}
