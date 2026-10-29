package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

func TestResponsesEndpointNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `data: {"id":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\ndata: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["object"] != "response" {
		t.Fatalf("out=%v", out)
	}
	if got := rec.Header().Get("X-Wb-Account"); got != "u1" {
		t.Fatalf("X-Wb-Account=%q want u1", got)
	}
}

func TestResponsesEndpointStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	if got := rec.Header().Get("X-Wb-Account"); got != "u1" {
		t.Fatalf("X-Wb-Account=%q want u1", got)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] frames=%d want 1 body=%s", done, rec.Body)
	}
	responsesFindEvent(t, events, "response.completed")
	if responsesCountEvent(events, "response.failed") != 0 {
		t.Fatalf("unexpected response.failed in %v", responsesEventTypes(events))
	}
}

func TestResponsesEndpointRecordsUsageAndSticky(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"u1"} },
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseWithCredit, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	raw := `{"model":"glm-5.2","input":"hi","stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	per1k, ok := p.ModelCost("u1", "glm-5.2")
	if !ok || per1k != 1.0 {
		t.Fatalf("ModelCost(u1,glm-5.2)=%v ok=%v want 1.0 true", per1k, ok)
	}
	used, _, _ := h.budget.snapshot()
	if used != 2.0 {
		t.Fatalf("budget used=%v want 2.0", used)
	}
	chatBody, _, err := buildChatBodyFromResponses([]byte(raw))
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	key := session.StickyFallbackKey(chatBody)
	if key == "" {
		t.Fatal("expected a conversation-derived sticky key")
	}
	if uid, ok := st.lastUID(key); !ok || uid != "u1" {
		t.Fatalf("sticky binding=%q ok=%v want u1 (binds=%v)", uid, ok, st.binds)
	}
}

func TestResponsesEndpointStreamUsageBookkeeping(t *testing.T) {
	const sseUsageThenError = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":1000,\"credit\":2.0}}\n\n" +
		"data: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_error\",\"code\":\"6004\"}}\n\n"

	t.Run("success records completion tokens and credit", func(t *testing.T) {
		resetMetricsForTest(t)
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			return 200, sseWithCredit, true
		})
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
			strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		snap := MetricsSnapshotOf()
		if snap.Total.CompletionTokens != 1000 {
			t.Fatalf("completion_tokens=%d want 1000 (completion-only, not prompt+completion)", snap.Total.CompletionTokens)
		}
		if snap.Total.PromptTokens != 1000 {
			t.Fatalf("prompt_tokens=%d want 1000", snap.Total.PromptTokens)
		}
		if snap.Total.Credit != 2.0 {
			t.Fatalf("credit=%v want 2.0", snap.Total.Credit)
		}
		per1k, ok := p.ModelCost("u1", "glm-5.2")
		if !ok || per1k != 1.0 {
			t.Fatalf("ModelCost(u1,glm-5.2)=%v ok=%v want 1.0 true", per1k, ok)
		}
		used, _, _ := h.budget.snapshot()
		if used != 2.0 {
			t.Fatalf("budget used=%v want 2.0", used)
		}
	})

	t.Run("failed stream still records credit", func(t *testing.T) {
		resetMetricsForTest(t)
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			return 200, sseUsageThenError, true
		})
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
			strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		events, _ := parseResponsesSSE(t, rec.Body.String())
		responsesFindEvent(t, events, "response.failed")
		per1k, ok := p.ModelCost("u1", "glm-5.2")
		if !ok || per1k != 1.0 {
			t.Fatalf("ModelCost(u1,glm-5.2)=%v ok=%v want 1.0 true", per1k, ok)
		}
		used, _, _ := h.budget.snapshot()
		if used != 2.0 {
			t.Fatalf("budget used=%v want 2.0", used)
		}
		st, _ := p.Status("u1")
		if st.SuccessCount != 0 {
			t.Fatalf("failed stream must not mark success, success_count=%d", st.SuccessCount)
		}
	})
}

func TestResponsesEndpointStreamFailureNoDoubleWrite(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] frames=%d want 1 body=%s", strings.Count(body, "data: [DONE]"), body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("unexpected write after [DONE]: %q", body)
	}
	events, done := parseResponsesSSE(t, body)
	if done != 1 {
		t.Fatalf("[DONE] frames=%d want 1", done)
	}
	responsesFindEvent(t, events, "response.failed")
	if responsesCountEvent(events, "response.completed") != 0 {
		t.Fatalf("unexpected response.completed in %v", responsesEventTypes(events))
	}
}

func TestResponsesTooManyImagesShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResponsesTooManyImages(rec, 3, 1)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413", rec.Code)
	}
	var out struct {
		Error struct {
			Type       string `json:"type"`
			Code       string `json:"code"`
			Param      string `json:"param"`
			ImageCount int    `json:"image_count"`
			MaxImages  int    `json:"max_images"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid error json: %v body=%s", err, rec.Body)
	}
	if out.Error.Type != "invalid_request_error" || out.Error.Code != "too_many_images" {
		t.Fatalf("error=%+v body=%s", out.Error, rec.Body)
	}
	if out.Error.Param != "input" || out.Error.ImageCount != 3 || out.Error.MaxImages != 1 {
		t.Fatalf("error fields=%+v body=%s", out.Error, rec.Body)
	}
}

func TestResponsesEndpointErrorShape(t *testing.T) {
	type errorEnvelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}

	t.Run("invalid json", func(t *testing.T) {
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			t.Fatalf("upstream must not be called for invalid JSON")
			return 500, "", false
		})
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{`)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var out errorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("invalid error json: %v body=%s", err, rec.Body)
		}
		if out.Error.Type != "invalid_request_error" {
			t.Fatalf("error.type=%q want invalid_request_error body=%s", out.Error.Type, rec.Body)
		}
		if out.Error.Code != "invalid_request" {
			t.Fatalf("error.code=%q want invalid_request body=%s", out.Error.Code, rec.Body)
		}
	})

	t.Run("no healthy account", func(t *testing.T) {
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			t.Fatalf("upstream must not be called without accounts")
			return 500, "", false
		})
		h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
			strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var out errorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("invalid error json: %v body=%s", err, rec.Body)
		}
		if out.Error.Type != "server_error" {
			t.Fatalf("error.type=%q want server_error body=%s", out.Error.Type, rec.Body)
		}
		if out.Error.Code != "no_healthy_account" {
			t.Fatalf("error.code=%q want no_healthy_account body=%s", out.Error.Code, rec.Body)
		}
	})

	t.Run("payload too large", func(t *testing.T) {
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			t.Fatalf("upstream must not be called for an oversized body")
			return 500, "", false
		})
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 16})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
			strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var out errorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("invalid error json: %v body=%s", err, rec.Body)
		}
		if out.Error.Code != "payload_too_large" {
			t.Fatalf("error.code=%q want payload_too_large body=%s", out.Error.Code, rec.Body)
		}
	})
}

func TestResponsesErrorType(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusServiceUnavailable, "server_error"},
		{http.StatusBadGateway, "server_error"},
		{http.StatusInternalServerError, "api_error"},
	}
	for _, tc := range cases {
		if got := responsesErrorType(tc.status); got != tc.want {
			t.Errorf("responsesErrorType(%d)=%q want %q", tc.status, got, tc.want)
		}
	}
}
