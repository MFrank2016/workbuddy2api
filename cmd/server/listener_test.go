package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func testServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}
}

func TestListenerApplyMovesAndKeepsOldOnFailure(t *testing.T) {
	m := NewListenerManager(testServer)
	if err := m.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	old := m.Addr()
	if old == "" {
		t.Fatal("empty addr")
	}
	if err := m.Apply("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if m.Addr() == old {
		t.Fatal("addr did not change")
	}
	// 占用一个端口，Apply 必须失败且保留当前监听。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	before := m.Addr()
	if err := m.Apply(ln.Addr().String()); err == nil {
		t.Fatal("must fail on occupied port")
	}
	if m.Addr() != before {
		t.Fatal("current listener must survive failed apply")
	}
}

func TestSameListen(t *testing.T) {
	if !sameListen("[::]:7863", ":7863") {
		t.Fatal("wildcard v6 and empty host should match")
	}
	if sameListen("127.0.0.1:7863", ":7863") {
		t.Fatal("specific host must not match wildcard")
	}
	if sameListen("127.0.0.1:7863", "127.0.0.1:7864") {
		t.Fatal("different ports must not match")
	}
}

// 默认窗口必须覆盖实测的 10–45s 流，否则容器重建时在途 SSE 仍会被掐断。
func TestShutdownGraceCoversLongStreams(t *testing.T) {
	if shutdownGrace < 120*time.Second {
		t.Fatalf("shutdownGrace=%v want >= 120s (SSE streams run 10–45s)", shutdownGrace)
	}
}

// Close 必须等在途请求自然结束，而不是关掉 listener 就走。
func TestCloseWaitsForInFlightRequest(t *testing.T) {
	old := shutdownGrace
	shutdownGrace = 500 * time.Millisecond
	defer func() { shutdownGrace = old }()

	started := make(chan struct{})
	m := NewListenerManager(func(addr string) *http.Server {
		return &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte("ok"))
		})}
	})
	if err := m.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + m.Addr() + "/")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{body: string(b), err: err}
	}()

	<-started
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("in-flight request failed: %v", got.err)
	}
	if got.body != "ok" {
		t.Fatalf("body=%q want ok", got.body)
	}
}
