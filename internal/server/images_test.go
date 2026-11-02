package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestImagesGenerations OpenAI 形状进出：上游 data 原样透出。
func TestImagesGenerations(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"created":1,"data":[{"url":"https://cos.example/x.png"}],"usage":{"credit":5.71}}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Created int `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Created != 1 || len(out.Data) != 1 || out.Data[0].URL != "https://cos.example/x.png" {
		t.Fatalf("bad shape: %s", rec.Body)
	}
}

// TestImagesMissingPrompt 缺 prompt → 400，不打上游。
func TestImagesMissingPrompt(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, `{}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if calls != 0 {
		t.Errorf("missing prompt must not reach upstream, calls=%d", calls)
	}
}

// TestImagesNoAccountForRealm 该 realm 无可用账号 → 503（不回落另一域）。
func TestImagesNoAccountForRealm(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{}`, false
	})
	// 池里只有 CN 账号，请求 global 域。
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"global:hy-image-3.5","prompt":"cat"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
}
