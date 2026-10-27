package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

type anthropicStreamReader struct {
	io.Reader
	io.Closer
}

// anthropicErrorType maps an HTTP status to the Anthropic error type.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	case http.StatusBadGateway:
		return "api_error"
	default:
		return "api_error"
	}
}

// writeAnthropicError writes the canonical Anthropic error envelope.
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

// writeAnthropicErrorStatus derives the Anthropic error type from the status code
// and attaches the gateway hint when present.
func writeAnthropicErrorStatus(w http.ResponseWriter, status int, code, msg, hint string) {
	e := map[string]any{
		"type":    anthropicErrorType(status),
		"message": msg,
	}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": e})
}

func writeAnthropicTooManyImages(w http.ResponseWriter, imageCount, maxImages int) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type": "invalid_request_error",
			"message": fmt.Sprintf("request contains %d images, which exceeds upstream.max_images=%d; reduce the number of images in input",
				imageCount, maxImages),
			"image_count": imageCount,
			"max_images":  maxImages,
		},
	})
}

func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if !h.budget.admit() {
		used, limit, _ := h.budget.snapshot()
		writeAnthropicErrorStatus(w, http.StatusTooManyRequests, "daily_budget_exceeded",
			fmt.Sprintf("daily credit budget exhausted (used %.2f of %.2f, resets at 00:00 CST)", used, limit), "")
		return
	}
	raw, err := h.readBody(w, r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAnthropicErrorStatus(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds the gateway limit (server.max_body_mb)",
				"raise server.max_body_mb, or set it to 0 to disable the limit")
			return
		}
		writeAnthropicErrorStatus(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error(), "")
		return
	}
	chatBody, meta, err := buildChatBodyFromAnthropic(raw)
	if err != nil {
		writeAnthropicErrorStatus(w, http.StatusBadRequest, "invalid_request", err.Error(), "")
		return
	}
	st := newChatStat(time.Now(), raw, meta.Stream, h.budget)
	defer st.done()
	prep, perr := h.prepareChatRequest(r, ChatExecOptions{
		Body:     chatBody,
		Stream:   meta.Stream,
		Protocol: "anthropic",
		Stat:     st,
		OnError: func(status int, code, msg, hint string) {
			writeAnthropicErrorStatus(w, status, code, msg, hint)
		},
		OnAccount: func(a *auth.Auth) { w.Header().Set("X-Wb-Account", a.UID) },
	})
	if perr != nil {
		st.status = perr.Status
		if perr.Code == "too_many_images" {
			writeAnthropicTooManyImages(w, perr.ImageCount, perr.MaxImages)
			return
		}
		writeAnthropicErrorStatus(w, perr.Status, perr.Code, perr.Msg, perr.Hint)
		return
	}
	ex, ok := h.executeChat(r.Context(), r, prep)
	if !ok {
		return
	}
	defer ex.Release()

	recordCredit := func(credit float64, total int, hasCredit bool) {
		if !hasCredit {
			return
		}
		st.credit = credit
		st.hasCredit = true
		h.cfg.Pool.NoteModelCost(ex.Account.UID, ex.BareModel, credit, total)
	}

	markSuccess := func() {
		h.cfg.Pool.NoteSuccess(ex.Account.UID)
		h.cfg.Pool.ClearModelEntitlement(ex.Realm, ex.BareModel)
		h.cfg.Pool.BlockModelClear(ex.Account.UID, ex.BareModel)
		if prep.StickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(prep.StickyKey, ex.Account.UID)
		}
	}

	if meta.Stream {
		st.status = http.StatusOK
		stats := newChatStatsReaderSince(ex.RC, st.start)
		rc := anthropicStreamReader{Reader: stats, Closer: ex.RC}
		streamErr := streamAnthropic(w, rc, meta)
		st.ttfb = stats.TTFB()
		st.prompt = stats.PromptTokens()
		st.cacheHit, st.cacheMiss, st.cacheWr, st.hasCache = stats.CacheTotals()
		credit, hasCredit := stats.Credit()
		h.recordChatAttempt(st, chatKeyName(r), ex.BareModel, prep.Peek.Model, ex.ModelRate,
			ex.Account.UID, stats.Usage(), credit, hasCredit, ex.AttemptStarted, stats.TTFB())
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(ex.BareModel, int64(st.prompt), int64(st.cacheHit), int64(st.cacheMiss))
		}
		recordCredit(credit, stats.TotalTokens(), hasCredit)
		if streamErr != nil {
			st.status = http.StatusBadGateway
			return
		}
		if toks, hasUsage := stats.Tokens(); hasUsage {
			st.toks = toks
		}
		markSuccess()
		return
	}

	credit, total, hasCredit := usageCreditTotal(ex.Aggregated)
	if usage, ok := ex.Aggregated["usage"].(map[string]any); ok {
		if hit, miss, okCache := cacheTokensFromUsage(usage); okCache {
			st.cacheHit, st.cacheMiss, st.hasCache = int(hit), int(miss), true
		}
	}
	h.recordChatAttempt(st, chatKeyName(r), ex.BareModel, prep.Peek.Model, ex.ModelRate,
		ex.Account.UID, usageDeltaFromResponse(ex.Aggregated), credit, hasCredit, ex.AttemptStarted, 0)
	if st.hasCache {
		cacheMissWarn.noteCacheTokens(ex.BareModel, int64(st.prompt), int64(st.cacheHit), int64(st.cacheMiss))
	}
	st.toks = completionTokens(ex.Aggregated)
	recordCredit(credit, total, hasCredit)
	markSuccess()
	writeJSON(w, http.StatusOK, buildAnthropicMessage(ex.Aggregated, meta))
	st.status = http.StatusOK
}

func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := h.readBody(w, r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAnthropicErrorStatus(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds the gateway limit (server.max_body_mb)",
				"raise server.max_body_mb, or set it to 0 to disable the limit")
			return
		}
		writeAnthropicErrorStatus(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error(), "")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicErrorStatus(w, http.StatusBadRequest, "invalid_request",
			"request body is not valid JSON: "+err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicTokens(req)})
}
