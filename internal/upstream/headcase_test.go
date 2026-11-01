package upstream

import (
	"net/http/httptest"
	"testing"
)

// TestNormalizeHeaderSpelling 逐名映射：官方全小写/保留大写的头必须按真机拼写
// 写出；Go canonical 形式（Acp-Connection-Id / X-B3-Parentspanid）不得出现。
func TestNormalizeHeaderSpelling(t *testing.T) {
	req := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	req.Header.Set("X-Request-Id", "r1")          // Go canonical 形式
	req.Header.Set("Acp-Connection-Id", "c1")     // Go canonical 形式
	req.Header.Set("X-B3-ParentSpanId", "p1")     // Go canonical 形式
	req.Header.Set("X-Stainless-Arch", "x64")     // Go canonical 形式
	req.Header.Set("Baggage", "b")                // Go canonical 形式
	req.Header.Set("X-Conversation-Id", "conv1")  // 官方 X-Conversation-ID
	req.Header.Set("User-Agent", "WorkBuddy/1.0") // 特判头：不得被改
	req.Header.Set("X-Unknown-Header", "keep")    // 表外头：原样

	out := normalizeHeaderSpelling(req)
	want := map[string]string{
		"X-Request-ID":        "r1",
		"acp-connection-id":   "c1",
		"X-B3-ParentSpanId":   "p1",
		"x-stainless-arch":    "x64",
		"baggage":             "b",
		"X-Conversation-ID":   "conv1",
		"User-Agent":          "WorkBuddy/1.0",
		"X-Unknown-Header":    "keep",
	}
	for k, v := range want {
		got := out.Header[k]
		if len(got) != 1 || got[0] != v {
			t.Errorf("header %q = %v, want [%q]", k, got, v)
		}
	}
	// Go canonical 形式的旧 key 必须消失（否则 wire 上会同时出现两种拼写）。
	for _, old := range []string{"X-Request-Id", "Acp-Connection-Id", "X-B3-Parentspanid", "X-Stainless-Arch", "Baggage", "X-Conversation-Id"} {
		if _, ok := out.Header[old]; ok {
			t.Errorf("old canonical key %q must be replaced, got %v", old, out.Header)
		}
	}
	// 返回副本：原请求不被改写（调用方/测试可能回读）。
	if _, ok := req.Header["X-Request-Id"]; !ok {
		t.Error("normalizeHeaderSpelling must not mutate the original request")
	}
}

// TestNormalizeHeaderSpellingSpecialCases 六个 Go 特判头保持 canonical 拼写，
// 否则 User-Agent 会回落成 Go-http-client/1.1、Content-Length 重复发送、
// Transport 再补一个 Accept-Encoding。
func TestNormalizeHeaderSpellingSpecialCases(t *testing.T) {
	req := httptest.NewRequest("POST", "https://example.com/", nil)
	req.Header.Set("User-Agent", "WorkBuddy/5.9.3")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	out := normalizeHeaderSpelling(req)
	for k, v := range map[string]string{
		"User-Agent":      "WorkBuddy/5.9.3",
		"Accept-Encoding": "gzip",
		"Content-Type":    "application/json",
	} {
		if got := out.Header[k]; len(got) != 1 || got[0] != v {
			t.Errorf("%s = %v want [%s]", k, got, v)
		}
	}
}
