package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestChatLargePromptRateLimitedDoesNotRotate 超大 prompt 撞 429 → 不换号重放，
// 等该号软冷却到期后同号重试一轮；仍失败则回 429。
//
// 抓的断裂：轮转对大小 prompt 一视同仁，一条 900k 上下文撞限流后会再向 2 个账号
// 各发一遍完整上下文（MaxRotate=3）。上游看到同一段私有对话几分钟内跨账号到达，
// 判「异常行为」并批量封号（2026-11-07 事故：23/31 个账号，重试放大 81M tokens）。
func TestChatLargePromptRateLimitedDoesNotRotate(t *testing.T) {
	var calls []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls = append(calls, authz)
		return 429, `{"code":429,"msg":"too many requests"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 2000) // u1 先被选中
	h := NewHandler(Config{
		Pool:                 p,
		Upstream:             up,
		LargePromptThreshold: 1000, // 阈值压到 1000 tokens，便于构造
		LargePromptMaxWait:   10 * time.Millisecond,
	})
	big := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + strings.Repeat("x", 8000) + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big)))

	// 必须回 429（不是 503 no_healthy_account）。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	// 关键：只打过 u1，**没有**把同一份上下文重放给 u2。
	for _, a := range calls {
		if a == "Bearer at-u2" {
			t.Fatalf("大 prompt 撞限流后不应换号重放，calls=%v", calls)
		}
	}
	if len(calls) == 0 || calls[0] != "Bearer at-u1" {
		t.Fatalf("应只打 u1，calls=%v", calls)
	}
}

// TestChatSmallPromptRateLimitStillRotates 小 prompt 撞 429 → 仍按原语义换号
// （大 prompt 保护不能误伤普通对话的可用性）。
func TestChatSmallPromptRateLimitStillRotates(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 429, `{"code":429,"msg":"too many requests"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 2000)
	h := NewHandler(Config{
		Pool:                 p,
		Upstream:             up,
		LargePromptThreshold: 100000, // 远高于本请求
		LargePromptMaxWait:   10 * time.Millisecond,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("小 prompt 应换号后成功，code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2（换号一次）", calls)
	}
}

// TestChatLargePromptRetriesSameAccountAfterCooldown 大 prompt 撞 429 → 等冷却后
// **同号**重试并成功；全程只碰一个账号。
//
// 抓的断裂：等待逻辑写了但不真的重试（或重试时换到了别的号）——那样「不跨号重放」
// 只是变成了「直接失败」，可用性白丢。
func TestChatLargePromptRetriesSameAccountAfterCooldown(t *testing.T) {
	var calls []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls = append(calls, authz)
		if len(calls) == 1 {
			return 429, `{"code":429,"msg":"too many requests"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 2000)
	h := NewHandler(Config{
		Pool:                 p,
		Upstream:             up,
		LargePromptThreshold: 1000,
		LargePromptMaxWait:   2 * time.Second,
		SoftCooldown:         50 * time.Millisecond, // 冷却短，等待可完成
	})
	big := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + strings.Repeat("x", 8000) + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（等待冷却后应同号重试成功）", rec.Code, rec.Body)
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%d want 2（同号重试一次）", len(calls))
	}
	if calls[0] != "Bearer at-u1" || calls[1] != "Bearer at-u1" {
		t.Fatalf("两次都必须打同一个号 u1，calls=%v", calls)
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
