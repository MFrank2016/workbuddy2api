package upstream

import (
	"testing"
	"time"
)

// TestClassifyQueueWaiting6020 6020 是渠道级排队（不是账号限流）：必须归
// ErrUpstreamBusy。按 ErrSoftRate 罚号会把几秒的排队放大成整段账号级冻结
// （2026-10-24 实案），且换号也撞同一堵墙（队列按 client_user_type 全局共享）。
func TestClassifyQueueWaiting6020(t *testing.T) {
	body := `{"code":6020,"msg":"queue.waiting.title","requestId":"01a181ca3b67934d01c3f64599c8490c","data":{"queue_position":2502,"queue_size":"999+","retry_after":40,"estimated_wait":12510}}`
	if got := Classify(429, body); got != ErrUpstreamBusy {
		t.Fatalf("Classify(429, 6020)=%v want ErrUpstreamBusy", got)
	}
	// 换状态码也认（防上游口径漂移）：结构化 code 是权威信号。
	if got := Classify(503, body); got != ErrUpstreamBusy {
		t.Fatalf("Classify(503, 6020)=%v want ErrUpstreamBusy", got)
	}
}

// TestParseQueueRetryAfter 6020/6021 的等待口径在 body（data.retry_after），
// 头侧没有；解析不到时调用方回落本地兜底。
func TestParseQueueRetryAfter(t *testing.T) {
	body := []byte(`{"code":6020,"data":{"retry_after":40}}`)
	d, ok := parseQueueRetryAfter(body)
	if !ok || d != 40*time.Second {
		t.Fatalf("parseQueueRetryAfter=%v/%v want 40s/true", d, ok)
	}
	if _, ok := parseQueueRetryAfter([]byte(`{"code":6020,"data":{}}`)); ok {
		t.Fatal("missing retry_after must return false")
	}
	big := []byte(`{"code":6020,"data":{"retry_after":999999}}`)
	if d, ok := parseQueueRetryAfter(big); !ok || d != retryAfterSanity {
		t.Fatalf("oversized retry_after=%v/%v want %v", d, ok, retryAfterSanity)
	}
}
