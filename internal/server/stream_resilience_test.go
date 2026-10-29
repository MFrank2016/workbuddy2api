package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/upstream"
)

// gatedBody 首次 Read 阻塞到 release 关闭，用来制造「首字节前」的静默期，
// 让心跳 goroutine 有机会运行。
type gatedBody struct {
	release <-chan struct{}
	started chan struct{}
	once    sync.Once
	r       io.Reader
}

func (b *gatedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.r.Read(p)
}

func (b *gatedBody) Close() error { return nil }

// TestLiveFallbackStreamResilience 锁住两条回落语义：Live 未接线时零值走 config
// 默认（15s / 开启）；Live 非空时以快照为准，0/false 能显式关闭。
func TestLiveFallbackStreamResilience(t *testing.T) {
	got := NewHandler(Config{}).loadLive()
	if got.StreamKeepalive != 15*time.Second {
		t.Errorf("zero Config StreamKeepalive=%v want 15s", got.StreamKeepalive)
	}
	if !got.FirstByteRetry {
		t.Error("zero Config FirstByteRetry must default true")
	}

	got = NewHandler(Config{StreamKeepalive: 7 * time.Second}).loadLive()
	if got.StreamKeepalive != 7*time.Second {
		t.Errorf("static StreamKeepalive=%v want 7s", got.StreamKeepalive)
	}

	live := livecfg.New(livecfg.Snapshot{StreamKeepalive: 0, FirstByteRetry: false})
	got = NewHandler(Config{Live: live}).loadLive()
	if got.StreamKeepalive != 0 {
		t.Errorf("Live StreamKeepalive=%v want 0", got.StreamKeepalive)
	}
	if got.FirstByteRetry {
		t.Errorf("Live FirstByteRetry=%v want false", got.FirstByteRetry)
	}
}

func heartbeatUpstream(release <-chan struct{}) (*upstream.Client, <-chan struct{}) {
	started := make(chan struct{})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &gatedBody{release: release, started: started, r: strings.NewReader(sseOK)},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return up, started
}

func runGatedChat(t *testing.T, h *Handler, release chan struct{}, started <-chan struct{}) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started reading upstream")
	}
	time.Sleep(80 * time.Millisecond)
	close(release)
	<-done
	return rec.Body.String()
}

// TestHeartbeatZeroDisablesE2E：stream_keepalive_seconds=0 时首字节前不得写任何注释心跳。
func TestHeartbeatZeroDisablesE2E(t *testing.T) {
	release := make(chan struct{})
	up, started := heartbeatUpstream(release)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Live:     livecfg.New(livecfg.Snapshot{StreamKeepalive: 0, FirstByteRetry: true}),
	})
	body := runGatedChat(t, h, release, started)
	if strings.Contains(body, ": keep-alive") {
		t.Fatalf("heartbeat must be disabled when keepalive=0: %q", body)
	}
}

// TestHeartbeatWiredFromConfig：非 0 周期应真的产生首字节前注释心跳（接线回归）。
func TestHeartbeatWiredFromConfig(t *testing.T) {
	release := make(chan struct{})
	up, started := heartbeatUpstream(release)
	h := NewHandler(Config{
		Pool:            testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:        up,
		StreamKeepalive: 20 * time.Millisecond,
		FirstByteRetry:  true,
	})
	body := runGatedChat(t, h, release, started)
	if !strings.Contains(body, ": keep-alive") {
		t.Fatalf("heartbeat not wired from config: %q", body)
	}
}

// TestReplayDisabledWhenFirstByteRetryFalse：显式关闭后首字节前传输错误不得重开上游。
func TestReplayDisabledWhenFirstByteRetryFalse(t *testing.T) {
	var calls int32
	const okSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			n := atomic.AddInt32(&calls, 1)
			var body io.ReadCloser
			if n == 1 {
				body = &firstReadErrBody{reader: strings.NewReader(""), err: io.ErrUnexpectedEOF}
			} else {
				body = io.NopCloser(strings.NewReader(okSSE))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		Live:     livecfg.New(livecfg.Snapshot{FirstByteRetry: false}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls=%d want 1 (replay disabled)", got)
	}
	if strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("replayed body leaked despite first_byte_retry=false: %s", rec.Body.String())
	}
}
