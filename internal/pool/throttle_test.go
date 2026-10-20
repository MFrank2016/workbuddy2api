package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func mkThrottleAuth(uid string, weight *float64, maxConc int, minInt float64) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", Domain: "copilot.tencent.com", Weight: weight, MaxConcurrency: maxConc, MinInterval: minInt}
}

func newThrottlePool(t *testing.T, rng func(n int64) int64) *Pool {
	t.Helper()
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.SetBreaker(3, time.Minute, time.Hour)
	p.SetMaxInFlight(10)
	p.SetMaxInFlightGlobal(10)
	p.SetRandomSource(rng)
	p.SetThrottleDefaults(1.0, 0, 0, 0)
	return p
}

func TestThrottleWeightZeroExcluded(t *testing.T) {
	p := newThrottlePool(t, nil)
	zero := 0.0
	p.Add(mkThrottleAuth("a", &zero, 0, 0))
	p.Add(mkThrottleAuth("b", nil, 0, 0))
	p.SetThrottleDefaults(1.0, 0, 0, 0)
	// weight=0 的 a 永不选到
	for i := 0; i < 50; i++ {
		if got := p.Pick(""); got == nil || got.UID == "a" {
			t.Fatalf("pick %d selected weight-0 account %v", i, got)
		}
	}
}

func TestThrottleWeightMultiplierBiases(t *testing.T) {
	withNoPickGap(t) // 关闭防撞号窗口，否则 heavy 首次选中后即被挤出后续 pick
	// 注入确定性随机源：恒返回 0 → 抽签选中第一个（权重最高者）
	p := newThrottlePool(t, func(n int64) int64 { return 0 })
	big := 100.0
	small := 0.01
	p.Add(mkThrottleAuth("heavy", &big, 0, 0))
	p.Add(mkThrottleAuth("light", &small, 0, 0))
	p.SetThrottleDefaults(1.0, 0, 0, 0)
	for i := 0; i < 20; i++ {
		if got := p.Pick(""); got == nil || got.UID != "heavy" {
			t.Fatalf("pick %d = %v, want heavy (rng=0 picks highest weight)", i, got)
		}
	}
}

func TestThrottleMinIntervalSkipsFreshAccount(t *testing.T) {
	// rng=0 → 首次恒选排序首位（等权重按 UID 稳定排序，a 先于 b）
	p := newThrottlePool(t, func(n int64) int64 { return 0 })
	p.Add(mkThrottleAuth("a", nil, 0, 60)) // min_interval 60s
	p.Add(mkThrottleAuth("b", nil, 0, 0))
	p.SetThrottleDefaults(1.0, 0, 0, 0)
	if got := p.Pick(""); got == nil || got.UID != "a" {
		t.Fatalf("first pick = %v, want a (never used)", got)
	}
	// a 刚被选，60s 间隔内再选 → b（a 在 min-interval 窗口内）
	if got := p.Pick(""); got == nil || got.UID != "b" {
		t.Fatalf("second pick = %v, want b (a in min-interval window)", got)
	}
}

func TestThrottleMaxConcurrencyPerAccount(t *testing.T) {
	p := newThrottlePool(t, nil)
	p.Add(mkThrottleAuth("a", nil, 1, 0)) // 单并发
	p.Add(mkThrottleAuth("b", nil, 0, 0))
	p.SetThrottleDefaults(1.0, 0, 0, 0)
	if !p.Acquire("a") {
		t.Fatal("acquire a #1 should succeed")
	}
	// a 的 in-flight=1 达上限（每号 max_concurrency=1）→ 选 b
	if got := p.Pick(""); got == nil || got.UID != "b" {
		t.Fatalf("pick = %v, want b (a at per-account concurrency cap)", got)
	}
	p.Release("a")
}

func TestThrottleTotalGateBlocksPick(t *testing.T) {
	p := newThrottlePool(t, nil)
	p.Add(mkThrottleAuth("a", nil, 0, 0))
	p.Add(mkThrottleAuth("b", nil, 0, 0))
	p.SetThrottleDefaults(1.0, 0, 0, 1) // 全池闸 1
	if !p.Acquire("a") {
		t.Fatal("acquire a")
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("pick = %v, want nil (pool gate saturated)", got)
	}
	p.Release("a")
	if got := p.Pick(""); got == nil {
		t.Fatal("pick after release should succeed")
	}
}

func TestThrottleTotalGateAcquire(t *testing.T) {
	p := newThrottlePool(t, nil)
	p.Add(mkThrottleAuth("a", nil, 0, 0))
	p.Add(mkThrottleAuth("b", nil, 0, 0))
	p.SetThrottleDefaults(1.0, 0, 0, 1) // 全池闸 1
	if !p.Acquire("a") {
		t.Fatal("acquire a #1")
	}
	// 全池已 1，再 acquire a（单号上限 10 未满）也应被总闸拒
	if p.Acquire("a") {
		t.Fatal("acquire a #2 should fail (pool gate 1 saturated)")
	}
	p.Release("a")
	if !p.Acquire("a") {
		t.Fatal("acquire a after release should succeed")
	}
}

func TestThrottleDefaultsZeroNoOp(t *testing.T) {
	// 所有默认零值 = 行为与改造前一致：weight 1.0 纯三因子、无限额。
	p := newThrottlePool(t, nil)
	p.Add(mkThrottleAuth("a", nil, 0, 0))
	p.Add(mkThrottleAuth("b", nil, 0, 0))
	p.SetThrottleDefaults(1.0, 0, 0, 0)
	// 两个 healthy 账号都能被选（不因新配置被排除）
	for i := 0; i < 10; i++ {
		if got := p.Pick(""); got == nil {
			t.Fatalf("pick %d = nil, want an account", i)
		}
	}
}
