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

func TestAnthropicMessagesEndpoint(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `data: {"id":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"type":"message"`) {
		t.Fatalf("body=%s", rec.Body)
	}
	var out struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if out.Type != "message" || len(out.Content) != 1 || out.Content[0].Text != "hi" {
		t.Fatalf("out=%+v", out)
	}
	if got := rec.Header().Get("X-Wb-Account"); got != "u1" {
		t.Fatalf("X-Wb-Account=%q want u1", got)
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_start", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in body=%s", want, body)
		}
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("unexpected error event body=%s", body)
	}
	if got := rec.Header().Get("X-Wb-Account"); got != "u1" {
		t.Fatalf("X-Wb-Account=%q want u1", got)
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Fatalf("count_tokens must not call upstream")
		return 500, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	raw := `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hello world"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	var req anthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if want := estimateAnthropicTokens(req); out.InputTokens != want {
		t.Fatalf("input_tokens=%d want %d", out.InputTokens, want)
	}
	if out.InputTokens <= 0 {
		t.Fatalf("input_tokens=%d want > 0", out.InputTokens)
	}
}

func TestAnthropicAuthErrorShape(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})

	assertAnthropicAuthError := func(t *testing.T, path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body)
		}
		var env struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s decode: %v body=%s", path, err, rec.Body)
		}
		if env.Type != "error" || env.Error.Type != "authentication_error" || env.Error.Message == "" {
			t.Fatalf("%s body=%s", path, rec.Body)
		}
	}

	assertAnthropicAuthError(t, "/v1/messages", `{"model":"glm-5.2","max_tokens":1,"messages":[]}`)
	assertAnthropicAuthError(t, "/v1/messages/count_tokens", `{"model":"glm-5.2","messages":[]}`)

	// Chat keeps its existing OpenAI-shaped auth failure.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body)
	}
	var chatEnv struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chatEnv); err != nil {
		t.Fatalf("chat decode: %v body=%s", err, rec.Body)
	}
	if chatEnv.Error.Type != "api_error" || chatEnv.Error.Code != "invalid_api_key" {
		t.Fatalf("chat auth shape changed: body=%s", rec.Body)
	}
}

func TestAnthropicRecordsUsageAndSticky(t *testing.T) {
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
	raw := `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw)))
	if rec.Code != http.StatusOK {
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
	chatBody, _, err := buildChatBodyFromAnthropic([]byte(raw))
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
