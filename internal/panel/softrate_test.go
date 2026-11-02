package panel

import (
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// TestSoftRateWaitUsesRetryAfterAndCap 等待时长：Retry-After 优先、缺省按次数递增、
// 两者都封顶 softRateGapMax（一个头不能把后台作业拖住）。
func TestSoftRateWaitUsesRetryAfterAndCap(t *testing.T) {
	withRetry := &upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: 30 * time.Second}
	if got := softRateWait(withRetry, 1); got != 30*time.Second {
		t.Errorf("Retry-After 优先：got %v want 30s", got)
	}
	if got := softRateWait(nil, 2); got != 2*softRateGap {
		t.Errorf("缺省递增：got %v want %v", got, 2*softRateGap)
	}
	huge := &upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: 10 * time.Minute}
	if got := softRateWait(huge, 1); got != softRateGapMax {
		t.Errorf("封顶：got %v want %v", got, softRateGapMax)
	}
}

// TestSoftRateGate 账号级冷却：记入后快失败，过期自动清。
func TestSoftRateGate(t *testing.T) {
	p := &Panel{}
	p.noteSoftRate("u1", 2*time.Second)
	if d := p.softRateGate("u1"); d <= 0 || d > 2*time.Second {
		t.Errorf("gate=%v want (0,2s]", d)
	}
	p.noteSoftRate("u2", -time.Second)
	if d := p.softRateGate("u2"); d != 0 {
		t.Errorf("过期应归零，got %v", d)
	}
	if d := p.softRateGate("unknown"); d != 0 {
		t.Errorf("unknown=%v want 0", d)
	}
}
