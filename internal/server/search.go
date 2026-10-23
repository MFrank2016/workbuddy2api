// search.go POST /v1/search 工具端点（契约 §4）。
//
// 端点职责刻意收得很窄：校验参数 → 解析 realm/profile/groups（pickForSearch，
// 见 searchpick.go）→ 进程内固定窗口限流（每上游 key 600/min）→ 选号 →
// 调 upstream.Search → 写 reqlog + usage。它**不**做模型路由、不读模型白名单
// （search 没有 model 维度），也不实现 MCP（Todo 27 复用 pickForSearch）。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/reqlog"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

const (
	// searchDefaultResults 请求未给 max_results 时的条数（契约 §4）。
	searchDefaultResults = 5
	// searchMaxResults max_results 上界；下界 1。越界本地 400，不夹取——
	// 静默夹取会让客户端以为参数生效（与 upstream.Search 的 clamp 兜底分工不同：
	// 那里是最后一道防御，这里是对外契约）。
	searchMaxResults = 10
	// searchMaxBody 请求体上限（search 请求只有 query + 一个整数，1MiB 远超所需）。
	searchMaxBody = 1 << 20
	// searchUpstreamCooldown 上游 429 后该账号的短冷却：search 是独立工具端点，
	// 不写模型级台账（"web_search" 不是模型），只做固定短冷却。
	searchUpstreamCooldown = 60 * time.Second

	// searchLogModel reqlog / usage 里的模型名（契约 §4 固定值）。
	searchLogModel = "web_search"
)

// searchResultItem 对外搜索结果条目（契约 §4 的三个字段）。
type searchResultItem struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// searchResponseBody 对外响应信封。
type searchResponseBody struct {
	Object  string             `json:"object"`
	Results []searchResultItem `json:"results"`
}

// gatedSearch search.enabled 闸在 withAuth **之外**：关闭时回纯文本 404，与
// 「路由未注册」完全一致（不向未鉴权探测暴露端点存在性）。开关走 livecfg，
// 面板热改即刻生效，故不能像 admin 那样在构造期条件注册。
func (h *Handler) gatedSearch(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().SearchEnabled {
		http.NotFound(w, r)
		return
	}
	h.withAuth(h.search)(w, r)
}

// search POST /v1/search 主流程。
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	obs := &responseObserver{ResponseWriter: w}
	start := time.Now()
	var accountLabel string
	if rec := h.cfg.RequestLog; rec != nil {
		rec.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			ms := time.Since(start).Milliseconds()
			if ms < 1 {
				ms = 1
			}
			rec.Record(reqlog.Event{
				Time:       start,
				Path:       "/v1/search",
				Account:    accountLabel,
				Model:      searchLogModel,
				Status:     status,
				OK:         status >= 200 && status < 300,
				HasCredit:  false,
				DurationMs: ms,
			})
		}()
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, searchMaxBody))
	if err != nil {
		writeOpenAIError(obs, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var in struct {
		Query      string `json:"query"`
		MaxResults *int   `json:"max_results"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeOpenAIError(obs, http.StatusBadRequest, "invalid_request",
			"request body is not valid JSON: "+err.Error())
		return
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		writeOpenAIError(obs, http.StatusBadRequest, "invalid_request",
			"query is required and must be a non-empty string")
		return
	}
	maxResults := searchDefaultResults
	if in.MaxResults != nil {
		if *in.MaxResults < 1 || *in.MaxResults > searchMaxResults {
			writeOpenAIError(obs, http.StatusBadRequest, "invalid_request",
				"max_results must be between 1 and 10")
			return
		}
		maxResults = *in.MaxResults
	}

	key := authKeyFrom(r.Context())
	pick, perr := pickForSearch(r, key)
	if perr != nil {
		var pe *searchPickError
		if errors.As(perr, &pe) {
			writeOpenAIError(obs, pe.Status, pe.Code, pe.Msg)
			return
		}
		writeOpenAIError(obs, http.StatusBadRequest, "invalid_request", perr.Error())
		return
	}

	if !h.searchLimiter.allow(pick.RateKey, time.Now()) {
		obs.Header().Set("Retry-After", "60")
		writeOpenAIError(obs, http.StatusTooManyRequests, "rate_limit_exceeded",
			"search rate limit exceeded (600 requests/minute per api key)")
		return
	}

	keyName := "main"
	if key != nil {
		keyName = key.Name
		if keyName == "" {
			keyName = "unnamed"
		}
	}

	results, accountLabel, lastErr := h.searchWithAccount(pick, keyName, query, maxResults)
	if results != nil {
		out := searchResponseBody{Object: "search_result", Results: make([]searchResultItem, 0, len(results))}
		for _, r := range results {
			out.Results = append(out.Results, searchResultItem{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
		}
		writeJSON(obs, http.StatusOK, out)
		return
	}

	var uerr *upstream.Error
	if errors.As(lastErr, &uerr) {
		if uerr.Kind == upstream.ErrSoftRate {
			obs.Header().Set("Retry-After", "60")
			writeOpenAIError(obs, http.StatusTooManyRequests, "rate_limit_exceeded",
				"rate limited by upstream: "+uerr.Msg)
			return
		}
		writeOpenAIError(obs, http.StatusBadGateway, "upstream_error", uerr.Msg)
		return
	}
	if lastErr != nil {
		writeOpenAIError(obs, http.StatusBadGateway, "upstream_error", lastErr.Error())
		return
	}
	writeOpenAIError(obs, http.StatusServiceUnavailable, "no_healthy_account",
		"all accounts are temporarily unavailable, please retry later")
}

// searchWithAccount 选号并执行一次搜索（失败换号轮转）。
//
// 成功返回 results + 账号标签；全部失败返回 (nil, "", lastErr)。lastErr 为 nil
// 表示一个可用账号都没有（调用方回 503）。search 与 MCP tools/call 共用这一份
// 选号/刷新/冷却逻辑（契约 §4：MCP 的 tools/call 必须复用同一判据）。
//
// profile 只在 profiles 激活（enabled=true 且 detect_only=false）时参与选号；
// 否则恒传空，保持 flag-off 的等价行为（契约 §1）。
func (h *Handler) searchWithAccount(pick searchPick, keyName, query string, maxResults int) ([]upstream.SearchResult, string, error) {
	selProfile := pick.Profile
	if !h.profilesActive() {
		selProfile = ""
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealmGroupsProfiles(tried, "", pick.Realm, pick.Groups, selProfile)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if rerr := h.cfg.Upstream.RefreshToken(acct); rerr != nil {
				h.cfg.Pool.NoteRefreshFail(acct.UID, rerr)
				h.cfg.Pool.NoteError(acct.UID)
				h.cfg.Pool.Release(acct.UID)
				lastErr = rerr
				continue
			}
			h.cfg.Pool.NoteRefreshOK(acct.UID)
		}
		attemptStart := time.Now()
		results, serr := h.cfg.Upstream.Search(acct, query, maxResults)
		h.cfg.Pool.Release(acct.UID)
		h.recordSearchUsage(pick.Realm, acct.UID, keyName, time.Since(attemptStart), serr == nil)
		if serr != nil {
			lastErr = serr
			h.cfg.Pool.NoteError(acct.UID)
			var uerr *upstream.Error
			if errors.As(serr, &uerr) && uerr.Kind == upstream.ErrSoftRate {
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, searchUpstreamCooldown, "web_search 429")
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		return results, logfmt.Label(acct.UID, acct.Nickname), nil
	}
	return nil, "", lastErr
}

// recordSearchUsage 把一次上游搜索尝试记入用量时序（无 token，只有请求数与延迟）。
// ok=false 也计数：失败重试放大必须在用量视图里可见（与 chat 的 recordAttempt 同口径）。
func (h *Handler) recordSearchUsage(realm, uid, keyName string, latency time.Duration, ok bool) {
	if h.cfg.Usage == nil {
		return
	}
	ms := latency.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	h.cfg.Usage.AddKeyed(time.Now(), realm, uid, searchLogModel, keyName, usage.Delta{
		LatencyMs:  ms,
		HasLatency: true,
	}, ok)
}
