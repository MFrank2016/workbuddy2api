package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestFetchReturnsContent DeepSeek Fetch API 形状：{content,url,title}。
func TestFetchReturnsContent(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"url":"https://example.com","title":"Example","content":"# hello"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://example.com"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Content string `json:"content"`
		URL     string `json:"url"`
		Title   string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "# hello" || out.URL != "https://example.com" || out.Title != "Example" {
		t.Fatalf("bad shape: %s", rec.Body)
	}
}

// TestFetchRejectsNonHTTPS 非 http(s) URL 本地拒绝，不打上游。
func TestFetchRejectsNonHTTPS(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, `{}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"file:///etc/passwd"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if calls != 0 {
		t.Errorf("invalid url must not reach upstream, calls=%d", calls)
	}
}

// TestFetchMissingContentIs502 上游 200 但缺 content：按 502 报错，不伪造空正文。
func TestFetchMissingContentIs502(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"url":"https://example.com"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://example.com"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
}
