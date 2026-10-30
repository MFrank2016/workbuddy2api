package scheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func planWindow(day string) (time.Time, time.Time, time.Time, time.Time) {
	ws, _ := parseClock(day, "08:00")
	we, _ := parseClock(day, "09:30")
	rs, _ := parseClock(day, "09:30")
	re, _ := parseClock(day, "10:00")
	return ws, we, rs, re
}

func TestPlanStoreDeterministicAndDue(t *testing.T) {
	day := "2026-10-30"
	ws, we, rs, re := planWindow(day)
	now := ws.Add(-time.Minute)
	s := NewPlanStore("")
	p1 := s.Ensure("u1", day, now, ws, we, rs, re)
	p2 := s.Ensure("u1", day, now, ws, we, rs, re)
	if !p1.PlanAt.Equal(p2.PlanAt) || !p1.RetryAt.Equal(p2.RetryAt) {
		t.Fatal("plan not deterministic")
	}
	if p1.PlanAt.Before(ws) || !p1.PlanAt.Before(we) {
		t.Fatalf("planAt %v outside window", p1.PlanAt)
	}
	if p1.RetryAt.Before(rs) || !p1.RetryAt.Before(re) {
		t.Fatalf("retryAt %v outside window", p1.RetryAt)
	}
	if got := s.Due(ws.Add(-time.Second), re); len(got) != 0 {
		t.Fatalf("due before plan = %d", len(got))
	}
	if got := s.Due(p1.PlanAt, re); len(got) != 1 || got[0].UID != "u1" {
		t.Fatalf("due at plan = %+v", got)
	}
	s.MarkAttempt("u1", day, p1.PlanAt)
	if got := s.Due(p1.PlanAt.Add(time.Minute), re); len(got) != 0 {
		t.Fatalf("due after first attempt = %d", len(got))
	}
	s.SetState("u1", day, "failed")
	if got := s.Due(p1.RetryAt, re); len(got) != 1 {
		t.Fatalf("due at retry = %d", len(got))
	}
	if got := s.Due(re.Add(time.Minute), re); len(got) != 0 {
		t.Fatalf("due after retry end = %d", len(got))
	}
}

func TestPlanStorePersists(t *testing.T) {
	day := "2026-10-30"
	ws, we, rs, re := planWindow(day)
	path := filepath.Join(t.TempDir(), "checkin-plan.json")
	s := NewPlanStore(path)
	p := s.Ensure("u1", day, ws, ws, we, rs, re)
	s.MarkAttempt("u1", day, p.PlanAt)
	s.SetState("u1", day, "done")
	reloaded := NewPlanStore(path)
	got, ok := reloaded.Get("u1", day)
	if !ok || got.Attempts != 1 || got.State != "done" {
		t.Fatalf("reloaded = %+v ok=%v", got, ok)
	}
}

func TestNextRandomCheckin(t *testing.T) {
	day := "2026-10-30"
	ws, _, _, re := planWindow(day)
	s := &Scheduler{checkinWindowStart: "08:00", checkinRetryEnd: "10:00"}
	if got := s.nextRandomCheckin(ws.Add(-time.Hour)); !got.Equal(ws) {
		t.Fatalf("before window = %v, want %v", got, ws)
	}
	if got := s.nextRandomCheckin(ws.Add(10 * time.Minute)); !got.Equal(ws.Add(11 * time.Minute)) {
		t.Fatalf("inside window = %v", got)
	}
	next, _ := parseClock("2026-10-31", "08:00")
	if got := s.nextRandomCheckin(re.Add(time.Minute)); !got.Equal(next) {
		t.Fatalf("after retry = %v, want %v", got, next)
	}
}
