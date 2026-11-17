package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 到期批次必须随账号状态一起持久化并透出到 /status：面板「积分到期提醒」重启后
// 仍要能渲染完整分日快照，不能等到下一次余额刷新才有数据。
func TestSetCreditsExpiryBucketsRoundTrip(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	now := time.Now()
	buckets := []ExpiryBucket{
		{Date: "2026-11-20", ExpireAt: now.Add(48 * time.Hour), Remain: 300},
		{Date: "2026-11-27", ExpireAt: now.Add(9 * 24 * time.Hour), Remain: 700},
	}
	p.SetCreditsDetailedWithBuckets("u1", 1000, 1500, 1000, buckets[0].ExpireAt, 300, buckets)

	st, _ := p.Status("u1")
	if len(st.CreditsExpiryBuckets) != 2 {
		t.Fatalf("status buckets = %+v, want 2", st.CreditsExpiryBuckets)
	}
	if st.CreditsExpiryBuckets[0].Remain != 300 || st.CreditsExpiryBuckets[1].Remain != 700 {
		t.Fatalf("status buckets = %+v, want 300/700", st.CreditsExpiryBuckets)
	}
	if st.CreditsExpiryBuckets[0].Date != "2026-11-20" {
		t.Fatalf("first bucket date = %q, want 2026-11-20", st.CreditsExpiryBuckets[0].Date)
	}

	// 持久化快照必须带上批次（重启后立即可渲染）。
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	acct, ok := sf.Accounts["u1"]
	if !ok {
		t.Fatal("state file missing u1")
	}
	if len(acct.CreditsExpiryBuckets) != 2 {
		t.Fatalf("persisted buckets = %+v, want 2", acct.CreditsExpiryBuckets)
	}
}

// 已过期与零剩余批次在恢复时惰性剔除：否则首页会把历史批次当成「即将到期」。
func TestExpiryBucketsDropStaleOnRestore(t *testing.T) {
	now := time.Now()
	in := []ExpiryBucket{
		{Date: "2020-01-01", ExpireAt: now.Add(-24 * time.Hour), Remain: 100}, // 已过期
		{Date: "2020-01-02", ExpireAt: now.Add(24 * time.Hour), Remain: 0},    // 零剩余
		{Date: "2026-11-20", ExpireAt: now.Add(48 * time.Hour), Remain: 300},  // 有效
	}
	out := liveExpiryBuckets(in, now)
	if len(out) != 1 || out[0].Remain != 300 {
		t.Fatalf("liveExpiryBuckets = %+v, want only the live bucket", out)
	}
}
