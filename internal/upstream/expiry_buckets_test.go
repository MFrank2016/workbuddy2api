package upstream

import (
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ExpiryBucket 单个自然日到期的积分合计：面板「积分到期提醒」需要按日分组，
// 只给「最早一批」无法表达「同一天多个包」「后续批次」「7 天内合计」。
func TestUserResourceExpiryBucketsGroupByDay(t *testing.T) {
	now := time.Now()
	day := func(offset int) string {
		return now.AddDate(0, 0, offset).Format(packageEndLayout)
	}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"A","CycleEndTime":"` + day(2) + `","CycleCapacitySize":100,"CycleCapacityRemain":100},` +
				`{"PackageName":"B","CycleEndTime":"` + day(2) + `","CycleCapacitySize":200,"CycleCapacityRemain":200},` +
				`{"PackageName":"C","CycleEndTime":"` + day(10) + `","CycleCapacitySize":300,"CycleCapacityRemain":300}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	// soon=0 = 不按窗口裁剪，返回全部未来批次（面板要展示所有分日批次）。
	buckets, err := c.UserResourceExpiryBuckets(a, 0)
	if err != nil {
		t.Fatalf("expiry buckets: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %+v, want 2 days", buckets)
	}
	// 同日两包合并为一条，金额相加；日期升序。
	if buckets[0].Remain != 300 {
		t.Errorf("buckets[0].Remain = %d, want 300 (same-day packages summed)", buckets[0].Remain)
	}
	if buckets[1].Remain != 300 {
		t.Errorf("buckets[1].Remain = %d, want 300", buckets[1].Remain)
	}
	if buckets[0].Date >= buckets[1].Date {
		t.Errorf("buckets not ascending by date: %s >= %s", buckets[0].Date, buckets[1].Date)
	}
	// ExpireAt 是该日最早一批的精确时刻，供「最早到期优先」使用。
	if buckets[0].ExpireAt.IsZero() {
		t.Error("buckets[0].ExpireAt is zero, want the day's earliest expiry")
	}
}

// 已过期的包、零余额的包都不进快照：它们不构成「即将到期」的提醒。
func TestUserResourceExpiryBucketsSkipExpiredAndEmpty(t *testing.T) {
	now := time.Now()
	day := func(offset int) string {
		return now.AddDate(0, 0, offset).Format(packageEndLayout)
	}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"past","CycleEndTime":"` + day(-3) + `","CycleCapacitySize":100,"CycleCapacityRemain":100},` +
				`{"PackageName":"empty","CycleEndTime":"` + day(3) + `","CycleCapacitySize":100,"CycleCapacityRemain":0},` +
				`{"PackageName":"live","CycleEndTime":"` + day(3) + `","CycleCapacitySize":100,"CycleCapacityRemain":50}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	// 窗口内（7 天）：已过期与零余额都不进快照。
	buckets, err := c.UserResourceExpiryBuckets(a, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("expiry buckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Remain != 50 {
		t.Fatalf("buckets = %+v, want only the live package", buckets)
	}
}
