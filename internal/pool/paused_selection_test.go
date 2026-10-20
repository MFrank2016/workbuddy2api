// paused_selection_test.go 钉住「暂停选号」的完整语义（panel 移植后的关键回归）：
// paused 账号必须退出**全部选号路径**（普通轮换 / 全冷却兜底 / 粘性命中 / 模型豁免
// / ServableNow 探活），但保号任务（签到/活跃/保活/余额刷新）照常执行——后者的
// 门控在 scheduler 侧，与本文件的选号排除互为正反面。
package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestPausedExcludedFromAllPickPaths 单一账号 paused 后，所有选号入口都必须返回 nil。
// 若 healthy()/fallback/modelExempt 任一漏检 paused，本用例即失败（历史 bug：
// Pause 置位后账号仍被 Pick 选中）。
func TestPausedExcludedFromAllPickPaths(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

	if !p.Pause("u1") {
		t.Fatal("pause u1 failed")
	}
	// 状态透出（面板据此显示「已暂停选号」）。
	if st, ok := p.Status("u1"); !ok || !st.Paused {
		t.Fatalf("Status.Paused = %+v, want true", st)
	}
	// 普通轮换（含全冷却兜底路径）。
	if got := p.Pick(""); got != nil {
		t.Fatalf("paused account must not be picked, got %s", got.UID)
	}
	// 粘性命中 / 定址路径。
	if got := p.PickByUIDForModel("u1", ""); got != nil {
		t.Fatalf("paused account must not be sticky-picked, got %s", got.UID)
	}
	// 模型非空（健康口径走 healthyForModel）。
	if got := p.PickByUIDForModel("u1", "glm-5.2"); got != nil {
		t.Fatalf("paused account must not be sticky-picked for model, got %s", got.UID)
	}
	// 池可用性（/healthz 探活）：paused 号不能把池「救活」。
	if p.ServableNow() {
		t.Fatal("paused-only pool must not be servable")
	}
	// 恢复后全部路径回池。
	if !p.Resume("u1") {
		t.Fatal("resume u1 failed")
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("resumed account must be pickable")
	}
	if !p.ServableNow() {
		t.Fatal("resumed pool must be servable")
	}
}

// TestDisableClearsPaused 禁用是比暂停更强的终态：Disable 后 paused 位必须一并清除，
// 避免 revive 后出现「已复活但仍被 paused 挡住」的隐性残留。
func TestDisableClearsPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Pause("u1")
	p.Disable("u1", "test disable")
	if st, ok := p.Status("u1"); !ok || st.Paused {
		t.Fatalf("after Disable: Paused=%v, want false（禁用应清除暂停位）", st.Paused)
	}
}
