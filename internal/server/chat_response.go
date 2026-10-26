package server

import (
	"log"
	"net/http"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// streamChatResponse 透传上游流，并在流尾按实际结果处置账号。
func (h *Handler) streamChatResponse(w http.ResponseWriter, r *http.Request, prep *preparedChat, ex *ChatExecution) {
	st := ex.Stat
	acct := ex.Account
	rc := ex.RC
	realm := ex.Realm
	bareModel := ex.BareModel
	stickyKey := prep.StickyKey
	declaredTools := prep.DeclaredTools
	keyName := chatKeyName(r)
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		h.recordChatAttempt(st, keyName, bareModel, prep.Peek.Model, ex.ModelRate, uid, delta, credit, hasCredit, started, ttfb)
	}
	// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
	// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
	// 此前在读第一帧之前就 NoteSuccess + 清 11102 负缓存 + 绑粘性 → 被限流的号
	// 记成健康、粘性把会话钉死在它身上，后续每一轮都打同一个限流号。
	//
	// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
	st.status = http.StatusOK
	stats := newChatStatsReaderSince(rc, st.start)
	// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
	// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
	// 组装请求上下文做判定）。
	// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
	var errFrame string
	// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
	// 这里在透传前还原成 delta.tool_calls（见 upstream/dsml.go）。
	// 第二个参数为 true = 名单为空时启用弱判定——tools 声明在链路上丢失是
	// 实测最常见的泄漏成因，只做严格判定等于对主场景不设防。
	repair := upstream.NewMarkupRepair(declaredTools, true)
	sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
		return h.hintContext(realm, bareModel, prep.ReqHasImage)
	}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
		upstream.WithMarkupRepair(repair))
	switch {
	case upstream.IsEmptyStreamError(sErr):
		// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
		// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
		// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
		// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
		// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
		// 502 观测没有意义）。
		st.status = http.StatusBadGateway
		log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
	case upstream.IsTruncatedStreamError(sErr) && errFrame == "":
		// `&& errFrame == ""`：上游 200 + error 帧后直接关 TCP（6004 限流 /
		// 审核拦截的典型形态）也会被判成截断。那种情况必须走下面的 error 帧
		// 分支做账号处置（模型级冷却 / 惩罚 / 清负缓存），否则限流号会继续
		// 被选中。
		// 上游 200 但流中途断掉（转发过内容帧、无 finish_reason、无 [DONE]）：
		// StreamHint 已在 [DONE] 之前补一条 error 帧（客户端由此判失败而非
		// 「完成」）。这里同样把日志/状态收敛到 502 —— 不这么做，日志里留下
		// 的是一条 tok=... 的假成功，正是本问题此前查不出来的原因。
		st.status = http.StatusBadGateway
		log.Printf("WARN: [server] stream acct=%s model=%s: upstream truncated (no finish_reason/[DONE])", logfmt.Label(acct.UID, acct.Nickname), bareModel)
	case errFrame != "":
		// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
		// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
		// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
		// 整个会话钉在它身上，后续每轮都失败。
		kind := upstream.FrameKind(errFrame)
		h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil)
		st.status = http.StatusServiceUnavailable
		log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
			logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(errFrame, 200))
	case sErr != nil:
		// 客户端写失败（断连）：上游帧无恙，账号健康——账号侧照常记成功
		// （与 default 同语义），请求日志归为中断（人已走，未完成）。
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.ClearModelEntitlement(realm, bareModel)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if stickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(stickyKey, acct.UID)
		}
	default:
		// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.ClearModelEntitlement(realm, bareModel)
		// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
		// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if stickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(stickyKey, acct.UID)
		}
	}
	if n := repair.Converted(); n > 0 {
		// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
		// 打一行便于运维确认「这次不再空转」以及统计发生率。
		log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
			logfmt.Label(acct.UID, acct.Nickname), bareModel, n, len(declaredTools))
	} else if m := repair.Seen(); m > 0 {
		// 识别到标记却没还原：说明判定没过（工具名不合规 / 块内夹带正文 /
		// 参数畸形）。单独记一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
		log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
			logfmt.Label(acct.UID, acct.Nickname), bareModel, m, len(declaredTools))
	}
	st.ttfb = stats.TTFB()
	// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
	// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
	// 与非流式走 completionTokens 返回 -1 的口径不一致。
	if toks, hasUsage := stats.Tokens(); hasUsage {
		st.toks = toks
	}
	// metrics 采集：token 三段 + 缓存三段 + 真实扣费（供 /v1/stats）。
	// 与成本账本同源同口径（都读末帧 usage），故此处一并带出，避免二次解析。
	st.prompt = stats.PromptTokens()
	st.cacheHit, st.cacheMiss, st.cacheWr, st.hasCache = stats.CacheTotals()
	// 账号级用量台账 + 用量时序：本次尝试的 token/耗时/TPS 一次成账
	//（失败路径同样计入 attempts——重试放大在用量视图里可见）。
	credit, hasCredit := stats.Credit()
	recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, ex.AttemptStarted, stats.TTFB())
	// WARN 信号放 recordAttempt 之后：st.prompt 此时才是本次的观测值。
	if st.hasCache {
		cacheMissWarn.noteCacheTokens(bareModel, int64(st.prompt), int64(st.cacheHit), int64(st.cacheMiss))
	}
	// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
	// 供下次选号把免费/便宜的号排在前面。
	if hasCredit {
		st.credit = credit
		st.hasCredit = true
		h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
	} else if st.hasUsage {
		// R9(c) 防护观测：usage 存在但 credit 缺失（如 global SSE 末帧未带 credit）。
		// 不算合法成本观测（缺失≠0），仅记一条 WARN 协助排障，绝不写入账本。
		log.Printf("WARN: [server] stream usage without credit acct=%s model=%s (no cost observation)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
	}
	rc.Close()
}

// writeChatAggregate 写出非流式聚合响应，并记录用量/成本与粘性。
func (h *Handler) writeChatAggregate(w http.ResponseWriter, r *http.Request, prep *preparedChat, ex *ChatExecution) {
	st := ex.Stat
	acct := ex.Account
	realm := ex.Realm
	bareModel := ex.BareModel
	stickyKey := prep.StickyKey
	declaredTools := prep.DeclaredTools
	keyName := chatKeyName(r)
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		h.recordChatAttempt(st, keyName, bareModel, prep.Peek.Model, ex.ModelRate, uid, delta, credit, hasCredit, started, ttfb)
	}
	resp := ex.Aggregated
	// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
	// 只在「没有结构化 tool_calls」且「finish_reason 为 stop」时生效（见 dsml.go）；
	// 名单为空时走弱判定（tools 声明在中转环节丢失是常态）。
	mrepair := upstream.RepairAggregatedResponse(resp, declaredTools, true)
	if n := mrepair.Converted(); n > 0 {
		log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
			bareModel, n, len(declaredTools))
	} else if m := mrepair.Seen(); m > 0 {
		log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
			bareModel, m, len(declaredTools))
	}
	credit, total, hasCredit := usageCreditTotal(resp)
	if usage, ok := resp["usage"].(map[string]any); ok {
		if hit, miss, okC := cacheTokensFromUsage(usage); okC {
			st.cacheHit, st.cacheMiss, st.hasCache = int(hit), int(miss), true
		}
	}
	recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, ex.AttemptStarted, 0)
	// WARN 信号放 recordAttempt 之后：st.prompt 此时才是本次的观测值。
	if st.hasCache {
		cacheMissWarn.noteCacheTokens(bareModel, int64(st.prompt), int64(st.cacheHit), int64(st.cacheMiss))
	}
	writeJSON(w, http.StatusOK, resp)
	st.status = http.StatusOK
	st.toks = completionTokens(resp)
	// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
	h.cfg.Pool.NoteSuccess(acct.UID)
	h.cfg.Pool.ClearModelEntitlement(realm, bareModel)
	h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
	if stickyKey != "" && h.cfg.Session != nil {
		h.cfg.Session.Bind(stickyKey, acct.UID)
	}
	// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
	if hasCredit {
		st.credit = credit
		st.hasCredit = true
		h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
	}
}
