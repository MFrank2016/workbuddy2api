package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

type responsesStreamReader struct {
	io.Reader
	io.Closer
}

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

func writeResponsesTooManyImages(w http.ResponseWriter, imageCount, maxImages int) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
		"error": map[string]any{
			"message": fmt.Sprintf("request contains %d images, which exceeds upstream.max_images=%d; reduce the number of images in input",
				imageCount, maxImages),
			"type":        "invalid_request_error",
			"code":        "too_many_images",
			"param":       "input",
			"image_count": imageCount,
			"max_images":  maxImages,
		},
	})
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	if !h.budget.admit() {
		used, limit, _ := h.budget.snapshot()
		writeResponsesErrorStatus(w, http.StatusTooManyRequests, "daily_budget_exceeded",
			fmt.Sprintf("daily credit budget exhausted (used %.2f of %.2f, resets at 00:00 CST)", used, limit), "")
		return
	}
	raw, err := h.readBody(w, r)
	if err == nil {
		h.captureInbound(r, raw)
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeResponsesErrorStatus(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds the gateway limit (server.max_body_mb)",
				"raise server.max_body_mb, or set it to 0 to disable the limit")
			return
		}
		writeResponsesErrorStatus(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error(), "")
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
		OnAccount:    func(a *auth.Auth) { w.Header().Set("X-Wb-Account", a.UID) },
		OnRetryAfter: func(secs int) { w.Header().Set("Retry-After", fmt.Sprintf("%d", secs)) },
	})
	if perr != nil {
		st.status = perr.Status
		if perr.Code == "too_many_images" {
			writeResponsesTooManyImages(w, perr.ImageCount, perr.MaxImages)
			return
		}
		writeResponsesErrorStatus(w, perr.Status, perr.Code, perr.Msg, perr.Hint)
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
		rc := responsesStreamReader{Reader: stats, Closer: ex.RC}
		streamErr := streamResponses(w, rc, meta)
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
	writeJSON(w, http.StatusOK, buildResponsesObject(ex.Aggregated, meta))
	st.status = http.StatusOK
}
