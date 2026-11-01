package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// modelAwareUpstream 记录上游实际收到的 model（从请求 body 解析），并按 model
// 返回不同响应。newFakeUpstream 只看 Authorization 头，无法断言「降级后上游收到
// 的模型真的变了」——那正是 model_fallback 的核心验收点。
type modelAwareUpstream struct {
	mu   sync.Mutex
	seen []string
}

func (m *modelAwareUpstream) models() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.seen))
	copy(out, m.seen)
	return out
}

func newModelAwareUpstream(t *testing.T, reply func(model string) (status int, body string)) (*upstream.Client, *modelAwareUpstream) {
	t.Helper()
	m := &modelAwareUpstream{}
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &req)
			m.mu.Lock()
			m.seen = append(m.seen, req.Model)
			m.mu.Unlock()
			status, body := reply(req.Model)
			ct := "application/json"
			if status == 200 {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return c, m
}

const modelFallback6004Body = `{"code":6004,"msg":"您的使用量已超出频率限制"}`

// TestModelFallbackOn6004 6004 模型级限流时同账号改道等价模型：上游应依次收到
// [原模型, 等价模型]，最终 200。
func TestModelFallbackOn6004(t *testing.T) {
	up, seen := newModelAwareUpstream(t, func(model string) (int, string) {
		if model == "glm-5.2" {
			return 429, modelFallback6004Body
		}
		return 200, sseOK
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		ModelsFallback: map[string]string{"glm-5.2": "glm-5.3"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200（降级后应成功）", rec.Code, rec.Body)
	}
	if got := seen.models(); len(got) != 2 || got[0] != "glm-5.2" || got[1] != "glm-5.3" {
		t.Fatalf("上游收到的模型=%v want [glm-5.2 glm-5.3]", got)
	}
	// 冷却记在原模型名下：等价模型不得被冷却。
	st, _ := p.Status("u1")
	for _, m := range st.RateLimitedModels {
		if m.Model == "glm-5.3" {
			t.Errorf("等价模型不应被冷却: %+v", st.RateLimitedModels)
		}
	}
}

// TestModelFallbackRealmPrefixedKey 带域前缀的键只在该域生效。
func TestModelFallbackRealmPrefixedKey(t *testing.T) {
	up, seen := newModelAwareUpstream(t, func(model string) (int, string) {
		if model == "glm-5.2" {
			return 429, modelFallback6004Body
		}
		return 200, sseOK
	})
	p := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:           p,
		Upstream:       up,
		ModelsFallback: map[string]string{"global:glm-5.2": "glm-5.3"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200（前缀键应触发 global 域降级）", rec.Code, rec.Body)
	}
	if got := seen.models(); len(got) != 2 || got[0] != "glm-5.2" || got[1] != "glm-5.3" {
		t.Fatalf("上游收到的模型=%v want [glm-5.2 glm-5.3]", got)
	}
}

// TestModelFallbackPrefixDoesNotLeakRealm cn 请求不得命中 global: 前缀键。
func TestModelFallbackPrefixDoesNotLeakRealm(t *testing.T) {
	up, seen := newModelAwareUpstream(t, func(model string) (int, string) {
		return 429, modelFallback6004Body
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:           p,
		Upstream:       up,
		ModelsFallback: map[string]string{"global:glm-5.2": "glm-5.3"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("cn 请求不应命中 global: 前缀键（body=%s）", rec.Body)
	}
	if got := seen.models(); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("上游收到的模型=%v want [glm-5.2]（不应降级）", got)
	}
}

// TestModelFallbackNoConfigKeepsOldBehavior 未配置时零回归：只打原模型一次。
func TestModelFallbackNoConfigKeepsOldBehavior(t *testing.T) {
	up, seen := newModelAwareUpstream(t, func(model string) (int, string) {
		return 429, modelFallback6004Body
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("未配置降级不应成功: %s", rec.Body)
	}
	if got := seen.models(); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("上游收到的模型=%v want [glm-5.2]", got)
	}
}

// TestModelFallbackNoLoop 等价模型也 6004 时不得 A→B→A 循环：每个目标最多试一次。
func TestModelFallbackNoLoop(t *testing.T) {
	up, seen := newModelAwareUpstream(t, func(model string) (int, string) {
		return 429, modelFallback6004Body
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:           p,
		Upstream:       up,
		ModelsFallback: map[string]string{"glm-5.2": "glm-5.3", "glm-5.3": "glm-5.2"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("两个模型都 6004 不应成功: %s", rec.Body)
	}
	if got := seen.models(); len(got) != 2 || got[0] != "glm-5.2" || got[1] != "glm-5.3" {
		t.Fatalf("上游收到的模型=%v want [glm-5.2 glm-5.3]（不得回环）", got)
	}
}
