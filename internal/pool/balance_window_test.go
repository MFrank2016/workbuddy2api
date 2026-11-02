package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestBalanceWindowAccumulatesAndExpires 窗口用量记账口径：累加、按模型独立、
// 跨窗口整体翻篇（不是逐次衰减）。
func TestBalanceWindowAccumulatesAndExpires(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.SetBalanceWindow(time.Hour)
	p.NoteModelCost("a1", "m", 0, 1000)
	p.NoteModelCost("a1", "m", 0, 500)

	p.mu.Lock()
	got := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now())
	if got != 1500 {
		t.Fatalf("窗口内用量应为 1500，得到 %d", got)
	}
	if other := p.byUID["a1"].windowTokens("other", p.balanceWindow, time.Now()); other != 0 {
		t.Fatalf("别的模型不该共享窗口用量，得到 %d", other)
	}
	if expired := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now().Add(2*time.Hour)); expired != 0 {
		t.Fatalf("窗口过期后应归零，得到 %d", expired)
	}
	p.mu.Unlock()

	// 注：上面用「模拟未来」只验证读取侧归零；真实窗口过期需要时间推进，
	// 这里只断言新记账不会把旧窗口的量翻倍累加（≤ 旧 + 新）。
	p.NoteModelCost("a1", "m", 0, 200)
	p.mu.Lock()
	if got := p.byUID["a1"].windowTokens("m", p.balanceWindow, time.Now()); got > 1500+200 {
		t.Fatalf("新记账不应超出旧窗口 + 本次，得到 %d", got)
	}
	p.mu.Unlock()
}

// TestBalanceWindowOffRecordsNothing 关停（0）时完全不记账。
func TestBalanceWindowOffRecordsNothing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.SetBalanceWindow(0)
	p.NoteModelCost("a1", "m", 0, 1000)
	p.mu.Lock()
	defer p.mu.Unlock()
	if got := p.byUID["a1"].windowTokens("m", time.Hour, time.Now()); got != 0 {
		t.Fatalf("关停后不该记账，得到 %d", got)
	}
}

// TestBalanceWindowLeansOffHotAccount 热点号被降权到次席；等量用量时回到 uid 兜底。
func TestBalanceWindowLeansOffHotAccount(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	newPool := func() *Pool {
		p := New("")
		p.SetBalanceWindow(time.Hour)
		p.SetCostExploreInterval(0)
		for _, uid := range []string{"a1", "a2"} {
			p.Add(&auth.Auth{UID: uid})
			p.SetCreditsDetailed(uid, 100, 100, 0, time.Time{}, 0)
			// 都留一笔免费观测：否则「无观测」方会被打进 tier 1，选号结果与用量无关。
			p.NoteModelCost(uid, "m", 0, 1)
			p.mu.Lock()
			p.byUID[uid].lastUsed = now.Add(-2 * time.Hour)
			p.mu.Unlock()
		}
		p.randInt64N = func(int64) int64 { return 0 }
		return p
	}

	if got := newPool().Pick("m"); got == nil || got.UID != "a1" {
		t.Fatalf("零用量基线应选 a1（uid 兜底），得到 %v", got)
	}

	p := newPool()
	for i := 0; i < 9; i++ {
		p.NoteModelCost("a1", "m", 0, 1000)
	}
	if got := p.PickExcludingForRealm(nil, "m", ""); got == nil || got.UID != "a2" {
		t.Fatalf("热点号应被降权到次席，得到 %v", got)
	}

	p2 := newPool()
	for i := 0; i < 9; i++ {
		p2.NoteModelCost("a1", "m", 0, 1000)
		p2.NoteModelCost("a2", "m", 0, 1000)
	}
	if got := p2.PickExcludingForRealm(nil, "m", ""); got == nil || got.UID != "a1" {
		t.Fatalf("等量用量时应回到 uid 兜底 a1，得到 %v", got)
	}
}
