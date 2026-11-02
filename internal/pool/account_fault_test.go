package pool

import (
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestNoteAccountFaultHoldEscalates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteAccountFault("u1", "11140 test") {
		t.Fatal("默认 hold 策略首次不应禁用")
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" || st.AccountFaultHolds != 1 {
		t.Fatalf("首次应为 soft_rate 长冷却: %+v", st)
	}
	if !strings.Contains(st.Reason, "auto-retry") {
		t.Errorf("reason=%q 应提示自动重试", st.Reason)
	}
	if p.NoteAccountFault("u1", "11140 test") {
		t.Fatal("第 2 次不应禁用")
	}
	if !p.NoteAccountFault("u1", "11140 test") {
		t.Fatal("第 3 次应升级永久禁用")
	}
	st, _ = p.Status("u1")
	if !st.Disabled {
		t.Fatalf("第 3 次应 disabled: %+v", st)
	}
	// 复活清零计数。
	p.Revive("u1")
	st, _ = p.Status("u1")
	if st.AccountFaultHolds != 0 || st.Disabled {
		t.Fatalf("revive 应清零: %+v", st)
	}
	// 成功清零计数。
	p.NoteAccountFault("u1", "11140 test")
	p.NoteSuccess("u1")
	st, _ = p.Status("u1")
	if st.AccountFaultHolds != 0 {
		t.Errorf("NoteSuccess 应清零 holds: %+v", st)
	}
}

func TestNoteAccountFaultLegacyImmediateDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetAccountFaultPolicy(0, 0)
	if !p.NoteAccountFault("u1", "11140 test") {
		t.Fatal("hold=0 应立即永久禁用")
	}
	st, _ := p.Status("u1")
	if !st.Disabled || !strings.Contains(st.DisabledReason, "re-login required") {
		t.Fatalf("应立即禁用并带 re-login 原因: %+v", st)
	}
}

func TestReenableIfCreditsKeepsAccountFaultHold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if p.NoteAccountFault("u1", "11140 test") {
		t.Fatal("首次 11140 不应禁用")
	}
	p.ReenableIfCredits("u1", 500)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("余额刷新不得清掉 11140 冷静期: %+v", st)
	}
	if st.AccountFaultHolds != 1 {
		t.Fatalf("余额刷新不得清零 11140 计数: %+v", st)
	}
	if got := p.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("冷静期账号不应压过健康账号: %+v", got)
	}
	if p.NoteAccountFault("u1", "11140 test") {
		t.Fatal("第 2 次不应禁用")
	}
	st, _ = p.Status("u1")
	if st.AccountFaultHolds != 2 {
		t.Fatalf("第 2 次计数应为 2: %+v", st)
	}
}
