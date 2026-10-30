package pool

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestAcquireWaitWaitsForRelease(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	if !p.Acquire("u1") {
		t.Fatal("first acquire")
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		p.Release("u1")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p.SetAcquirePolicy(500*time.Millisecond, 8)
	if !p.AcquireWait(ctx, "u1") {
		t.Fatal("should acquire after release")
	}
	p.Release("u1")
}

func TestAcquireWaitTimeout(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	p.SetAcquirePolicy(20*time.Millisecond, 8)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	if !p.Acquire("u1") {
		t.Fatal("first acquire")
	}
	if p.AcquireWait(context.Background(), "u1") {
		t.Fatal("must timeout")
	}
	if got := p.QueueDepth(); got != 0 {
		t.Fatalf("queue depth=%d want 0 after timeout", got)
	}
	p.Release("u1")
}

func TestAcquireWaitQueueFull(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	p.SetAcquirePolicy(time.Second, 0)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	if !p.Acquire("u1") {
		t.Fatal("first acquire")
	}
	// maxQueue=0 表示不限；改成 1 后第二个等待者应被拒绝。
	p.SetAcquirePolicy(time.Second, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- p.AcquireWait(ctx, "u1") }()
	time.Sleep(10 * time.Millisecond)
	if p.QueueDepth() != 1 {
		t.Fatalf("queue depth=%d want 1", p.QueueDepth())
	}
	if p.AcquireWait(context.Background(), "u1") {
		t.Fatal("queue full must reject")
	}
	cancel()
	<-done
	p.Release("u1")
}
