// search_qa_test.go 真实 HTTP 面 QA（httptest.NewServer over TCP，等价 curl -i）。
//
// 与 search_test.go 的 ResponseRecorder 断言分开：这里证明路由、状态行与响应头
// 在真实 TCP 连接上与单测一致（Retry-After 这类字段只在真响应里可见）。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/reqlog"
)

// TestSearchQARealSurface 真实 HTTP 面 QA（httptest.NewServer，等价 curl -i）：
// 合法请求 → 200 + results + reqlog 有 web_search；cn key + X-WB2A-Realm: global
// → 400 key_realm_mismatch；第 601 次 → 429 + Retry-After: 60。
//
// 用真实监听端口而非 httptest.ResponseRecorder：证明路由/头/状态行在 TCP 面上
// 与单测断言一致（含 Retry-After 这类只在真响应头里可见的字段）。
func TestSearchQARealSurface(t *testing.T) {
	var hits []searchHit
	rec := reqlog.New(reqlog.Config{})
	h := searchHandler(t, &hits)
	h.cfg.RequestLog = rec
	srv := httptest.NewServer(h)
	defer srv.Close()

	post := func(authz, body string, headers map[string]string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/search", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+authz)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		return resp
	}

	// happy
	resp := post("sk-search", `{"query":"golang","max_results":3}`, nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("QA happy: %s -> %s body=%s", resp.Proto, resp.Status, strings.TrimSpace(string(raw)))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"object":"search_result"`) ||
		!strings.Contains(string(raw), `"url":"https://e.x/1"`) {
		t.Fatalf("happy path failed: %d %s", resp.StatusCode, raw)
	}
	found := false
	for _, e := range rec.Snapshot().Recent {
		if e.Model == "web_search" && e.Status == 200 {
			found = true
		}
	}
	if !found {
		t.Errorf("reqlog missing web_search entry: %+v", rec.Snapshot().Recent)
	}

	// realm mismatch
	resp = post("sk-search", `{"query":"x"}`, map[string]string{"X-WB2A-Realm": "global"})
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("QA realm mismatch: %s -> %s body=%s", resp.Proto, resp.Status, strings.TrimSpace(string(raw)))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), `"code":"key_realm_mismatch"`) {
		t.Fatalf("realm mismatch failed: %d %s", resp.StatusCode, raw)
	}

	// 600/min：打满窗口后第 601 次 429（happy 已用掉 1 次，故再打 599 次）
	for i := 0; i < 599; i++ {
		r := post("sk-search", `{"query":"x"}`, nil)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d want 200", i+2, r.StatusCode)
		}
	}
	resp = post("sk-search", `{"query":"x"}`, nil)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("QA 601st: %s -> %s retry-after=%s body=%s", resp.Proto, resp.Status,
		resp.Header.Get("Retry-After"), strings.TrimSpace(string(raw)))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("601st: %d want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "60" {
		t.Errorf("Retry-After=%q want 60", resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(string(raw), `"code":"rate_limit_exceeded"`) {
		t.Errorf("601st body=%s want rate_limit_exceeded", raw)
	}
}
