// official_e2e_test.go 反 11-128 出站兼容端到端：official profile / identity / DSH。
//
// 走真实 Handler.ServeHTTP → 选号 → upstream.Client（fake transport），只捕获真正
// 发往上游的 body/headers，断言默认 legacy 形状不被 official/identity 误改。
package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// capturedChat 一次出站 chat 请求的捕获结果。
type capturedChat struct {
	body          []byte
	header        http.Header
	contentLength int64
}

// captureChatUpstream 返回捕获出站 chat 请求的 fake upstream。
func captureChatUpstream(t *testing.T, got *capturedChat) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got.header = canonicalHeaderCloneServer(r.Header)
			got.contentLength = r.ContentLength
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read outbound body: %v", err)
			}
			got.body = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func e2eCN(t *testing.T) *auth.Auth {
	t.Helper()
	return &auth.Auth{UID: "u-cn", AccessToken: "at-cn", ExpiresAt: 9999999999, Domain: "copilot.tencent.com"}
}

func decodeGzipBody(t *testing.T, body []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return raw
}

func postChat(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// TestOfficialProfileE2E：CN + official → gzip body、官方头齐全、旧头移除。
func TestOfficialProfileE2E(t *testing.T) {
	var got capturedChat
	up := captureChatUpstream(t, &got)
	h := NewHandler(Config{
		Pool:           testPoolWith(e2eCN(t)),
		Upstream:       up,
		RequestProfile: "official",
	})
	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body)
	}
	if enc := got.header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding=%q want gzip", enc)
	}
	if got.contentLength != int64(len(got.body)) {
		t.Fatalf("ContentLength=%d body len=%d", got.contentLength, len(got.body))
	}
	raw := decodeGzipBody(t, got.body)
	if !strings.Contains(string(raw), `"messages"`) {
		t.Fatalf("decompressed body=%s", raw)
	}
	if !strings.Contains(string(raw), `"stream_options":{"include_usage":true}`) {
		t.Fatalf("official body defaults missing: %s", raw)
	}
	if got.header.Get("X-Product") != "SaaS" {
		t.Fatalf("X-Product=%q want SaaS", got.header.Get("X-Product"))
	}
	for _, name := range []string{"Origin", "Referer", "X-Client-ID", "X-Client-Version", "X-Refresh-Token"} {
		if v := got.header.Get(name); v != "" {
			t.Fatalf("legacy header %s=%q must be removed", name, v)
		}
	}
}

// TestIdentityProfileE2E：identity 只替换 system/assistant/developer 文本，user 原样。
func TestIdentityProfileE2E(t *testing.T) {
	var got capturedChat
	up := captureChatUpstream(t, &got)
	up.SetIdentity(upstream.IdentityOptions{
		Enabled:      true,
		Target:       "WorkBuddy",
		Replacements: map[string]string{"Claude Code": "WorkBuddy"},
	})
	h := NewHandler(Config{Pool: testPoolWith(e2eCN(t)), Upstream: up})
	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[`+
		`{"role":"system","content":"You are Claude Code."},`+
		`{"role":"user","content":"Claude Code says hi."}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body)
	}
	raw := string(got.body)
	if !strings.Contains(raw, `"content":"You are WorkBuddy."`) {
		t.Fatalf("system identity not replaced: %s", raw)
	}
	if !strings.Contains(raw, `"content":"Claude Code says hi."`) {
		t.Fatalf("user content must stay untouched: %s", raw)
	}
	if strings.Contains(raw, "You are Claude Code.") {
		t.Fatalf("system identity leaked: %s", raw)
	}
}

// TestDSHProfileE2E：identity 开启时 DSH GUI 环境句归一化。
func TestDSHProfileE2E(t *testing.T) {
	var got capturedChat
	up := captureChatUpstream(t, &got)
	up.SetIdentity(upstream.IdentityOptions{
		Enabled:      true,
		Target:       "WorkBuddy",
		Replacements: map[string]string{"Claude": "WorkBuddy"},
	})
	h := NewHandler(Config{Pool: testPoolWith(e2eCN(t)), Upstream: up})
	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[`+
		`{"role":"system","content":"You are operating in the DSH GUI environment with window controls."},`+
		`{"role":"user","content":"keep this"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body)
	}
	raw := string(got.body)
	if !strings.Contains(raw, "You are a helpful assistant operating in a text interface.") {
		t.Fatalf("DSH sentence not normalized: %s", raw)
	}
	if strings.Contains(raw, "DSH GUI environment") {
		t.Fatalf("DSH GUI sentence leaked: %s", raw)
	}
}

// TestOfficialIntlStaysLegacy：INTL 账号即使配置 official，也不 gzip、不加官方头/默认 body。
func TestOfficialIntlStaysLegacy(t *testing.T) {
	var got capturedChat
	up := captureChatUpstream(t, &got)
	up.GlobalEnabled = true
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{
			UID: "u-intl", AccessToken: "at-intl", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai",
		}),
		Upstream:       up,
		RequestProfile: "official",
	})
	rec := postChat(h, `{"model":"global:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body)
	}
	if enc := got.header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("INTL must stay legacy, got Content-Encoding=%q", enc)
	}
	for _, name := range []string{"X-Private-Data", "X-Agent-Intent"} {
		if v := got.header.Get(name); v != "" {
			t.Fatalf("INTL must not send official header %s, got %q", name, v)
		}
	}
	if strings.Contains(string(got.body), `"max_tokens":128000`) {
		t.Fatalf("INTL must not receive official body defaults: %s", got.body)
	}
}
