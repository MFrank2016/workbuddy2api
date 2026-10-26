package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

func responsesErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return "server_error"
	default:
		return "api_error"
	}
}

func writeResponsesErrorStatus(w http.ResponseWriter, status int, code, msg, hint string) {
	e := map[string]any{
		"message": msg,
		"type":    responsesErrorType(status),
		"code":    code,
	}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	if !h.budget.admit() {
		used, limit, _ := h.budget.snapshot()
		writeResponsesErrorStatus(w, http.StatusTooManyRequests, "daily_budget_exceeded",
			fmt.Sprintf("daily credit budget exhausted (used %.2f of %.2f, resets at 00:00 CST)", used, limit), "")
		return
	}
	raw, err := h.readBody(w, r)
	if err != nil {
		writeResponsesErrorStatus(w, http.StatusBadRequest, "invalid_request", err.Error(), "")
		return
	}
	chatBody, meta, err := buildChatBodyFromResponses(raw)
	if err != nil {
		writeResponsesErrorStatus(w, http.StatusBadRequest, "invalid_request", err.Error(), "")
		return
	}
	st := newChatStat(time.Now(), raw, meta.Stream, h.budget)
	defer st.done()
	prep, perr := h.prepareChatRequest(r, ChatExecOptions{
		Body:     chatBody,
		Stream:   meta.Stream,
		Protocol: "responses",
		Stat:     st,
		OnError: func(status int, code, msg, hint string) {
			writeResponsesErrorStatus(w, status, code, msg, hint)
		},
		OnAccount: func(a *auth.Auth) { w.Header().Set("X-Wb-Account", a.UID) },
	})
	if perr != nil {
		st.status = perr.Status
		writeResponsesErrorStatus(w, perr.Status, perr.Code, perr.Msg, perr.Hint)
		return
	}
	ex, ok := h.executeChat(r.Context(), r, prep)
	if !ok {
		return
	}
	defer ex.Release()
	if meta.Stream {
		err := streamResponses(w, ex.RC, meta)
		if errors.Is(err, errResponsesStreamFailed) {
			st.status = http.StatusBadGateway
			return
		}
		if err != nil {
			st.status = http.StatusBadGateway
			return
		}
		st.status = http.StatusOK
		return
	}
	writeJSON(w, http.StatusOK, buildResponsesObject(ex.Aggregated, meta))
	st.status = http.StatusOK
}
