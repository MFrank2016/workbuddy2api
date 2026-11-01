package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/reqlog"
	"workbuddy2api/internal/upstream"
)

// ctxBlockingReader 阻塞到 ctx 取消再返回 ctx.Err()：模拟「上游正文还在流、
// 客户端先断开」的真实形态（前置 CDN 回源超时、AsyncOpenAI 类客户端 timeout、
// 用户点停止）。
type ctxBlockingReader struct{ ctx context.Context }

func (r ctxBlockingReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// TestChatSyncClientCancelIsNotUpstreamParse 钉住客户端断开的分支：上游正文流到
// 一半、客户端先掐断时，不再记 502 upstream_parse —— 上游当时在正常输出，网关
// 没出错。断言三件事：不写 502 响应体、流水行状态是 499、handler 不卡死。
func TestChatSyncClientCancelIsNotUpstreamParse(t *testing.T) {
	withChatLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	var once sync.Once
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			once.Do(func() { close(entered) })
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(ctxBlockingReader{ctx}),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	var out string
	go func() {
		out = captureStdout(t, func() { h.ServeHTTP(rec, req) })
		close(done)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler 未在 ctx 取消后返回（卡在读上游）")
	}
	if rec.Code == http.StatusBadGateway {
		t.Fatalf("调用方断开被记成 502 upstream_parse: %s", rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("连接已关，不应再写响应体: %s", rec.Body)
	}
	if !strings.Contains(out, "| 499 |") {
		t.Fatalf("流水行状态应为 499，实际输出:\n%s", out)
	}
}

// TestRequestTraceEventInterruptedOverride 归档口径：客户端中断时 responseObserver
// 捕获不到状态（连接已关、没有 WriteHeader），trace 的 statusOverride/outcome 必须
// 覆盖默认的 200/success，否则归档把「别人掐断」记成成功。
func TestRequestTraceEventInterruptedOverride(t *testing.T) {
	tr := &requestTrace{
		id:             "req-test",
		start:          time.Now(),
		outcome:        reqlog.OutcomeInterrupted,
		statusOverride: 499,
	}
	e := tr.event(http.StatusOK) // 模拟 observer 默认值
	if e.Status != 499 {
		t.Errorf("status=%d want 499", e.Status)
	}
	if e.Outcome != reqlog.OutcomeInterrupted {
		t.Errorf("outcome=%q want %s", e.Outcome, reqlog.OutcomeInterrupted)
	}
	if e.OK {
		t.Error("interrupted 不得记成 OK")
	}
}
