package livecfg

import (
	"testing"
	"time"
)

// TestNormalizeHiddenModels 归一化语义：
//   - nil/空/全空白 → nil（不隐藏）
//   - 裸名 → 两域都藏（"cn:x" + "global:x"）
//   - "cn:"/"global:" 前缀 → 只藏该域
//   - 非 cn/global 前缀（如 "xx:yy"）视为裸名整体（与 resolveModel 对称：
//     前缀不合法时 realm=cn、bare=原串，网关侧检查 "cn:xx:yy" 命中）
func TestNormalizeHiddenModels(t *testing.T) {
	if got := NormalizeHiddenModels(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{"  ", ""}); got != nil {
		t.Errorf("all blank → %v, want nil", got)
	}

	got := NormalizeHiddenModels([]string{"deepseek-v3"})
	for _, want := range []string{"cn:deepseek-v3", "global:deepseek-v3"} {
		if _, ok := got[want]; !ok {
			t.Errorf("bare name: missing %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("bare name: got %d entries %v, want 2", len(got), got)
	}

	got = NormalizeHiddenModels([]string{"cn:pro-model"})
	if _, ok := got["cn:pro-model"]; !ok {
		t.Errorf("cn: prefix: missing %q in %v", "cn:pro-model", got)
	}
	if _, ok := got["global:pro-model"]; ok {
		t.Errorf("cn: prefix: %q must not hide global realm", "global:pro-model")
	}

	// 前后空白被裁掉
	got = NormalizeHiddenModels([]string{"  glm-5.2  "})
	if _, ok := got["cn:glm-5.2"]; !ok {
		t.Errorf("trimmed bare name: missing %q in %v", "cn:glm-5.2", got)
	}

	// 非法前缀视为裸名整体
	got = NormalizeHiddenModels([]string{"xx:yy"})
	for _, want := range []string{"cn:xx:yy", "global:xx:yy"} {
		if _, ok := got[want]; !ok {
			t.Errorf("odd prefix: missing %q in %v", want, got)
		}
	}
}

// TestLiveSnapshotIdentityAndProfileFields 锁定 Task 1 新增的快照字段可经
// Store/Load 原样读出（映射由 cmd/server.liveSnapshot 负责）。
func TestLiveSnapshotIdentityAndProfileFields(t *testing.T) {
	s := Snapshot{
		RequestProfile:       "official",
		IdentityEnabled:      true,
		IdentityTarget:       "WorkBuddy",
		IdentityReplacements: map[string]string{"Claude": "WorkBuddy"},
	}
	got := New(s).Load()
	if got.RequestProfile != "official" {
		t.Errorf("RequestProfile=%q, want official", got.RequestProfile)
	}
	if !got.IdentityEnabled || got.IdentityTarget != "WorkBuddy" {
		t.Errorf("identity fields lost: %+v", got)
	}
	if got.IdentityReplacements["Claude"] != "WorkBuddy" {
		t.Errorf("IdentityReplacements=%v", got.IdentityReplacements)
	}
}

// TestLiveSnapshotStreamResilienceRoundTrip 锁定 Task 6 新增的两个流式韧性
// 字段能经 Store/Load 原样读出（映射由 cmd/server.liveSnapshot 负责）。
func TestLiveSnapshotStreamResilienceRoundTrip(t *testing.T) {
	s := Snapshot{StreamKeepalive: 7 * time.Second, FirstByteRetry: false}
	got := New(s).Load()
	if got.StreamKeepalive != 7*time.Second {
		t.Errorf("StreamKeepalive=%v want 7s", got.StreamKeepalive)
	}
	if got.FirstByteRetry {
		t.Errorf("FirstByteRetry=%v want false", got.FirstByteRetry)
	}
}
