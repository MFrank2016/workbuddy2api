// expiry_candidates_test.go 钉住到期分桶的「取最早候选」口径：
// get-user-resource 的每个套餐可能同时下发 CycleEndTime（周期边界，墙钟字符串）
// 与 DeductionEndTime（可抵扣窗口结束，epoch 毫秒）等候选字段。分桶必须取
// **所有可解析值中最早者**，并忽略零值/epoch 脏值（否则一个 0001-01-01 或 0
// 就能把整包误标成「快过期」，让选号白白插队）。
//
// 字段是否真实存在由录制 fixture 证明（testdata/get_user_resource_cn_recorded.json，
// 2026-10-22 CN 域实测响应），不是凭文档猜的。
package upstream

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// recordedResourceFixture 读取录制 fixture 的原始 JSON（字段存在性证明）。
func recordedResourceFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/get_user_resource_cn_recorded.json")
	if err != nil {
		t.Fatalf("读取录制 fixture: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析录制 fixture: %v", err)
	}
	return doc
}

// TestRecordedFixtureProvesExpiryFieldNames 用录制响应证明候选字段名真实存在：
//   - DeductionEndTime（epoch 毫秒）：CN/global 两域实测均下发；
//   - ExpiredTime：字段存在但恒为空串（实测 94/94 空）；
//   - PackageEndTime：**响应里不存在**（只在请求体过滤参数里出现），故分桶不能
//     依赖它——保留解析只为兼容/未来字段，不作为唯一判据。
func TestRecordedFixtureProvesExpiryFieldNames(t *testing.T) {
	doc := recordedResourceFixture(t)
	data, _ := doc["data"].(map[string]any)
	resp, _ := data["Response"].(map[string]any)
	body, _ := resp["Data"].(map[string]any)
	accounts, _ := body["Accounts"].([]any)
	if len(accounts) == 0 {
		t.Fatal("fixture 里没有 Accounts")
	}
	var withDeduction, withExpired, withPackageEnd, expiredNonEmpty int
	for _, raw := range accounts {
		acct, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("account 形态异常: %T", raw)
		}
		if v, ok := acct["DeductionEndTime"]; ok {
			withDeduction++
			if n, _ := v.(float64); n <= 0 {
				t.Errorf("fixture DeductionEndTime 非正数: %v", v)
			}
		}
		if v, ok := acct["ExpiredTime"]; ok {
			withExpired++
			if s, _ := v.(string); s != "" {
				expiredNonEmpty++
			}
		}
		if _, ok := acct["PackageEndTime"]; ok {
			withPackageEnd++
		}
	}
	if withDeduction != len(accounts) {
		t.Errorf("DeductionEndTime 应每个套餐都有：%d/%d", withDeduction, len(accounts))
	}
	if withExpired != len(accounts) {
		t.Errorf("ExpiredTime 字段应存在（值可为空）：%d/%d", withExpired, len(accounts))
	}
	if expiredNonEmpty != 0 {
		t.Errorf("实测 ExpiredTime 恒为空，fixture 却出现 %d 个非空值", expiredNonEmpty)
	}
	if withPackageEnd != 0 {
		t.Errorf("实测响应无 PackageEndTime 字段，fixture 却出现 %d 个", withPackageEnd)
	}
	// 结构体解析必须能读到这些字段（否则加字段等于没加）。
	// fixture 是上游**完整 HTTP body**（含 code/msg/data 信封），故用一层信封剥出
	// data 再交给 userResourceResp —— 与 doJSON 的真实解析路径一致。
	raw, err := os.ReadFile("testdata/get_user_resource_cn_recorded.json")
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	var envelope struct {
		Data userResourceResp `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("解析成 userResourceResp: %v", err)
	}
	accts := envelope.Data.Response.Data.Accounts
	if len(accts) == 0 {
		t.Fatal("userResourceResp 未解析出 Accounts（字段名/层级不对）")
	}
	if accts[0].DeductionEndTime <= 0 {
		t.Error("userResourceResp 未解析到 DeductionEndTime（字段名/类型不对）")
	}
	if accts[0].CycleEndTime == "" {
		t.Error("userResourceResp 未解析到 CycleEndTime")
	}
}

// mkResourceResp 构造 get-user-resource 响应（账号片段原样拼入）。
func mkResourceResp(accounts string) string {
	return `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[` + accounts + `]}}}}`
}

// itoa64 把 int64 渲染成 JSON 数字字面量（拼 fixture 用）。
func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// resourceBuckets 用给定账号片段跑一次分桶，返回 (remain, buckets)。
func resourceBuckets(t *testing.T, accounts string, soon time.Duration) (int64, CreditBuckets) {
	t.Helper()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(accounts), nil
	})
	remain, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, soon)
	if err != nil {
		t.Fatalf("UserResourceDetailed: %v", err)
	}
	return remain, buckets
}

// TestBucketingTakesEarliestCandidate DeductionEndTime 在 2034、CycleEndTime 在
// 3 天后 → 必须按 CycleEndTime（更早）分桶。若实现只认 DeductionEndTime，
// 快过期积分将永远进不了 Expiring，选号优先级失效。
func TestBucketingTakesEarliestCandidate(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	deduction2034 := time.Date(2034, 12, 19, 11, 37, 38, 0, time.UTC).UnixMilli()
	remain, buckets := resourceBuckets(t,
		`{"PackageName":"体验版","CycleEndTime":"`+in3d+`",`+
			`"DeductionEndTime":`+itoa64(deduction2034)+`,`+
			`"CycleCapacitySize":500,"CycleCapacityRemain":500,"CycleCapacityUsed":0}`,
		7*24*time.Hour)
	if remain != 500 {
		t.Errorf("remain=%d want 500（不得改变 remain 口径）", remain)
	}
	if buckets.Expiring != 500 {
		t.Errorf("expiring=%d want 500（CycleEndTime 更早，必须按它分桶）", buckets.Expiring)
	}
	if buckets.Stable != 0 {
		t.Errorf("stable=%d want 0", buckets.Stable)
	}
}

// TestBucketingOnlyDeductionEndTimeParseable 只有 DeductionEndTime 可解析（CycleEndTime
// 缺失/脏）时按它分桶：窗口内 → Expiring，窗口外 → Stable。
func TestBucketingOnlyDeductionEndTimeParseable(t *testing.T) {
	in3dMS := time.Now().Add(3 * 24 * time.Hour).UnixMilli()
	in60dMS := time.Now().Add(60 * 24 * time.Hour).UnixMilli()

	_, b1 := resourceBuckets(t,
		`{"PackageName":"奖励包","DeductionEndTime":`+itoa64(in3dMS)+`,`+
			`"CapacitySize":300,"CapacityRemain":200,"CapacityUsed":100}`,
		7*24*time.Hour)
	if b1.Expiring != 200 || b1.Stable != 0 {
		t.Errorf("仅 DeductionEndTime(3d) 可解析应进 Expiring，got %+v", b1)
	}

	_, b2 := resourceBuckets(t,
		`{"PackageName":"周期包","DeductionEndTime":`+itoa64(in60dMS)+`,`+
			`"CapacitySize":300,"CapacityRemain":300,"CapacityUsed":0}`,
		7*24*time.Hour)
	if b2.Expiring != 0 || b2.Stable != 300 {
		t.Errorf("仅 DeductionEndTime(60d) 可解析应留在 Stable，got %+v", b2)
	}
}

// TestBucketingIgnoresZeroAndEpoch 零值/epoch 到期字段必须被忽略：
// DeductionEndTime=0、ExpiredTime="0001-01-01 00:00:00"、PackageEndTime="1970-01-01 00:00:00"
// 都不能把整包误标成 Expiring（它们只表示「没有到期信息」或上游脏值）。
func TestBucketingIgnoresZeroAndEpoch(t *testing.T) {
	cases := []struct {
		name string
		acct string
	}{
		{"DeductionEndTime=0", `{"PackageName":"p","DeductionEndTime":0,"CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`},
		{"ExpiredTime=0001", `{"PackageName":"p","ExpiredTime":"0001-01-01 00:00:00","CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`},
		{"PackageEndTime=1970", `{"PackageName":"p","PackageEndTime":"1970-01-01 00:00:00","CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`},
		{"CycleEndTime=0001", `{"PackageName":"p","CycleEndTime":"0001-01-01 00:00:00","CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`},
		{"CycleEndTime=2020前", `{"PackageName":"p","CycleEndTime":"2019-12-31 23:59:59","CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`},
		{"DeductionEndTime=负数", `{"PackageName":"p","DeductionEndTime":-1,"CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, b := resourceBuckets(t, c.acct, 7*24*time.Hour)
			if b.Expiring != 0 {
				t.Errorf("脏到期值不得标 Expiring，got %+v", b)
			}
			if b.Stable != 80 {
				t.Errorf("stable=%d want 80", b.Stable)
			}
		})
	}
}

// TestBucketingDirtyFieldDoesNotPoisonValidOne 脏字段不能污染同包的有效字段：
// DeductionEndTime=0 但 CycleEndTime 在窗口内 → 仍应进 Expiring（取有效值中最早者）。
func TestBucketingDirtyFieldDoesNotPoisonValidOne(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	_, b := resourceBuckets(t,
		`{"PackageName":"p","DeductionEndTime":0,"CycleEndTime":"`+in3d+`",`+
			`"CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`,
		7*24*time.Hour)
	if b.Expiring != 80 || b.Stable != 0 {
		t.Errorf("有效 CycleEndTime 应生效（脏 DeductionEndTime 被忽略），got %+v", b)
	}
}

// TestBucketingAllUnparseableStable 全部候选都不可解析 → Stable（保守，不误标快过期）。
func TestBucketingAllUnparseableStable(t *testing.T) {
	_, b := resourceBuckets(t,
		`{"PackageName":"p","CycleEndTime":"not-a-time","ExpiredTime":"also-bad",`+
			`"DeductionEndTime":0,"CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`,
		7*24*time.Hour)
	if b.Expiring != 0 || b.Stable != 80 {
		t.Errorf("全不可解析应归 Stable，got %+v", b)
	}
}

// TestBucketingSoonDisabledAllStable soon<=0 时不分桶（旧行为逐字保留）。
func TestBucketingSoonDisabledAllStable(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in3dMS := time.Now().Add(3 * 24 * time.Hour).UnixMilli()
	_, b := resourceBuckets(t,
		`{"PackageName":"p","CycleEndTime":"`+in3d+`","DeductionEndTime":`+itoa64(in3dMS)+`,`+
			`"CapacitySize":100,"CapacityRemain":80,"CapacityUsed":20}`,
		0)
	if b.Expiring != 0 || b.Stable != 80 {
		t.Errorf("soon<=0 应全归 Stable，got %+v", b)
	}
}

// TestBucketingRealFixtureEarliestCandidate 真实录制 fixture 上跑一遍：
// 每个包都有 CycleEndTime + DeductionEndTime，取最早者分桶；不 panic、总数守恒。
func TestBucketingRealFixtureEarliestCandidate(t *testing.T) {
	raw, err := os.ReadFile("testdata/get_user_resource_cn_recorded.json")
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 fixture: %v", err)
	}
	// 直接复用解析后的 Accounts 片段构造响应体（保持字段原样）。
	data, _ := doc["data"].(map[string]any)
	resp, _ := data["Response"].(map[string]any)
	body, _ := resp["Data"].(map[string]any)
	accounts, _ := body["Accounts"].([]any)
	// mkDetailedResp 收的是「Accounts 数组内部片段」，故逐条 marshal 后用逗号拼接
	//（直接 marshal 整个数组会多一层 []，解析报 cannot unmarshal array into struct）。
	parts := make([]string, 0, len(accounts))
	for _, acct := range accounts {
		one, err := json.Marshal(acct)
		if err != nil {
			t.Fatalf("重打包 account: %v", err)
		}
		parts = append(parts, string(one))
	}
	remain, buckets := resourceBuckets(t, strings.Join(parts, ","), 7*24*time.Hour)
	if remain != buckets.Total() {
		t.Errorf("remain=%d != buckets.Total()=%d", remain, buckets.Total())
	}
	if remain <= 0 {
		t.Errorf("fixture 上 remain 应为正，got %d", remain)
	}
}
