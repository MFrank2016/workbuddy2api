package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
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
