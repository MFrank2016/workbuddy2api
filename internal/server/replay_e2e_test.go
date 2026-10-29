package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// firstReadErrBody 首次 Read 直接返回传输层错误，之后正常读取底流。
type firstReadErrBody struct {
	reader io.Reader
	err    error
	read   bool
}

func (b *firstReadErrBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return 0, b.err
	}
	return b.reader.Read(p)
}

func (b *firstReadErrBody) Close() error { return nil }

// TestReplayE2E 验证上游首字节前 EOF 时网关自动重开一次，客户端最终拿到完整 SSE。
func TestReplayE2E(t *testing.T) {
	var calls int
	const okSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			var body io.ReadCloser
			if calls == 1 {
				body = &firstReadErrBody{reader: strings.NewReader(""), err: io.ErrUnexpectedEOF}
			} else {
				body = io.NopCloser(strings.NewReader(okSSE))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)

	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 (first body errors, replay once)", calls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ok") {
		t.Fatalf("client did not receive ok content: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("client did not receive [DONE]: %s", body)
	}
}
