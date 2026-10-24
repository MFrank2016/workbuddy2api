// search_messages.go /v1/search/messages 的选号执行与 Anthropic 头解析。
//
// 上游把 runSearch 放在 server/search.go；本仓库的 search.go 已承载
// /v1/search + MCP 共用的 searchWithAccount，故把 Messages 端点的薄适配放这里，
// 避免两套选号逻辑漂移。
package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// searchFailure 搜索失败出口（供 Anthropic 错误形态映射）。
type searchFailure struct {
	status int
	msg    string
	hint   string
}

// searchMetaFromRequest 从 Anthropic 请求头取会话标识；缺 request id 时生成。
func searchMetaFromRequest(r *http.Request) upstream.ChatMeta {
	meta := upstream.ChatMeta{
		ConversationID:        strings.TrimSpace(r.Header.Get("X-Conversation-ID")),
		ConversationRequestID: strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID")),
		TraceID:               strings.TrimSpace(r.Header.Get("X-Trace-ID")),
	}
	if meta.ConversationRequestID == "" {
		meta.ConversationRequestID = session.NewMessageID()
	}
	return meta
}

// runSearch 执行一次搜索（与 /v1/search 共用 searchWithAccount 的选号/轮转）。
//
// 与上游版本的区别：不额外限流（messages 端点由 withAuth + 上游 key 限流覆盖），
// 不做 profile/realm 过滤（该端点没有 model/key 维度），直接使用全池。
func (h *Handler) runSearch(ctx context.Context, query string, maxResults int, meta upstream.ChatMeta) (*upstream.SearchOutcome, *searchFailure) {
	_ = ctx
	_ = meta
	results, _, err := h.searchWithAccount(searchPick{}, "main", query, maxResults)
	if results != nil {
		return &upstream.SearchOutcome{
			Query:        query,
			Results:      results,
			TotalResults: len(results),
		}, nil
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		status := ue.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return nil, &searchFailure{status: status, msg: ue.Msg}
	}
	if err != nil {
		return nil, &searchFailure{status: http.StatusBadGateway, msg: err.Error()}
	}
	return nil, &searchFailure{status: http.StatusServiceUnavailable,
		msg: "all accounts are temporarily unavailable, please retry later"}
}
