package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestSetPlan(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	now := time.Now()
	p.SetPlan("u1", Plan{Label: "高级版", CheckedAt: now})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.PlanLabel != "高级版" {
		t.Fatalf("plan=%q", st.PlanLabel)
	}
	if st.PlanCheckedAt.IsZero() {
		t.Fatal("plan checked_at missing")
	}
}
