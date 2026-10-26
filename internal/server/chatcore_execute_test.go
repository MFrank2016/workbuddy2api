package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestExecuteChatRotatesOnSoftRate(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 429, `{"code":429,"msg":"rate"}`, false
		}
		return 200, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	prep, err := h.prepareChatRequest(httptest.NewRequest("POST", "/v1/chat/completions", nil), ChatExecOptions{Body: []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	ex, ok := h.executeChat(context.Background(), httptest.NewRequest("POST", "/v1/chat/completions", nil), prep)
	if !ok || ex == nil {
		t.Fatal("expected success after rotation")
	}
	defer ex.Release()
	if calls != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
}

func TestChatNonStreamAggregateFailureKeepsAccountHeader(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Wb-Account"); got != "u1" {
		t.Fatalf("X-Wb-Account=%q want u1", got)
	}
}
