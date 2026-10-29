package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHeartbeatBeforeFirstData：首字节前按周期发 SSE 注释心跳，首帧数据后停止。
func TestHeartbeatBeforeFirstData(t *testing.T) {
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		_ = StreamHint(rec, pr, nil, WithHeartbeat(25*time.Millisecond))
		close(done)
	}()
	time.Sleep(65 * time.Millisecond)
	if _, err := pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, ": keep-alive\n\n") {
		t.Fatalf("no heartbeat before first data: %q", body)
	}
	if n := strings.Count(body, ": keep-alive"); n > 2 {
		t.Fatalf("heartbeat did not stop after first data (count=%d): %q", n, body)
	}
}

// TestNoHeartbeatAfterFirstData：数据帧先到，之后跨多个心跳周期不得再发注释。
func TestNoHeartbeatAfterFirstData(t *testing.T) {
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		_ = StreamHint(rec, pr, nil, WithHeartbeat(100*time.Millisecond))
		close(done)
	}()
	if _, err := pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := pw.Write([]byte("data: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if strings.Contains(body, ": keep-alive") {
		t.Fatalf("heartbeat sent after first data: %q", body)
	}
}

// TestHeartbeatStopsWhenStreamEnds：流结束（无数据）后心跳停止，StreamHint 返回后
// 不得再有任何写入（含收尾 error / [DONE] 之后）。
func TestHeartbeatStopsWhenStreamEnds(t *testing.T) {
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		_ = StreamHint(rec, pr, nil, WithHeartbeat(15*time.Millisecond))
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	_ = pw.Close()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, ": keep-alive") {
		t.Fatalf("heartbeat never ran before stream end: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing final DONE: %q", body)
	}
	time.Sleep(60 * time.Millisecond)
	if after := rec.Body.String(); after != body {
		t.Fatalf("heartbeat continued after stream end: %q -> %q", body, after)
	}
}
