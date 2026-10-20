package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func testRetryClient(t *testing.T, rt http.RoundTripper, retry RetryConfig) *Client {
	t.Helper()
	c := New()
	c.HTTP = &http.Client{Timeout: 5 * time.Second, Transport: rt}
	c.ChatHTTP = &http.Client{Timeout: 0, Transport: rt}
	c.Retry = retry
	return c
}

func newRetryTestAuth() *auth.Auth {
	return &auth.Auth{UID: "t1", AccessToken: "at", RefreshToken: "rt", Domain: "copilot.tencent.com"}
}

type countRT struct {
	count int32
	fn    func(n int) (*http.Response, error)
}

func (r *countRT) RoundTrip(req *http.Request) (*http.Response, error) {
	n := int(atomic.AddInt32(&r.count, 1))
	return r.fn(n)
}

func TestChatStreamRetry503Then200(t *testing.T) {
	rt := &countRT{fn: func(i int) (*http.Response, error) {
		if i == 1 {
			return &http.Response{StatusCode: 503, Status: "503", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`"msg":"boom"`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: ok\n\n"))}, nil
	}}
	c := testRetryClient(t, rt, RetryConfig{Enabled: true, Attempts: 1, Backoff: time.Millisecond})
	rc, status, _, err := c.ChatStreamContext(context.Background(), newRetryTestAuth(), []byte(`{}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if atomic.LoadInt32(&rt.count) != 2 {
		t.Errorf("request count = %d, want 2 (1 fail + 1 retry)", atomic.LoadInt32(&rt.count))
	}
}

func TestChatStreamRetryExhausted(t *testing.T) {
	rt := &countRT{fn: func(i int) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Status: "503", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`"msg":"boom"`))}, nil
	}}
	c := testRetryClient(t, rt, RetryConfig{Enabled: true, Attempts: 2, Backoff: time.Millisecond})
	_, status, _, err := c.ChatStreamContext(context.Background(), newRetryTestAuth(), []byte(`{}`), "", ChatMeta{})
	if status != 503 {
		t.Errorf("status = %d, want 503", status)
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrServer {
		t.Errorf("err = %v, want *Error{Kind: ErrServer}", err)
	}
	if atomic.LoadInt32(&rt.count) != 3 {
		t.Errorf("request count = %d, want 3 (1 + 2 attempts)", atomic.LoadInt32(&rt.count))
	}
}

func TestChatStreamRetrySkips429(t *testing.T) {
	rt := &countRT{fn: func(i int) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Status: "429", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":6004,"msg":"rate limited"}`))}, nil
	}}
	c := testRetryClient(t, rt, RetryConfig{Enabled: true, Attempts: 3, Backoff: time.Millisecond})
	_, _, _, err := c.ChatStreamContext(context.Background(), newRetryTestAuth(), []byte(`{}`), "", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrSoftRate {
		t.Errorf("err = %v, want ErrSoftRate", err)
	}
	if atomic.LoadInt32(&rt.count) != 1 {
		t.Errorf("request count = %d, want 1 (429 never retries)", atomic.LoadInt32(&rt.count))
	}
}

func TestChatStreamRetryTransportThenOK(t *testing.T) {
	rt := &countRT{fn: func(i int) (*http.Response, error) {
		if i == 1 {
			return nil, errors.New("connection reset by peer")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: ok\n\n"))}, nil
	}}
	c := testRetryClient(t, rt, RetryConfig{Enabled: true, Attempts: 1, Backoff: time.Millisecond})
	rc, status, _, err := c.ChatStreamContext(context.Background(), newRetryTestAuth(), []byte(`{}`), "", ChatMeta{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if atomic.LoadInt32(&rt.count) != 2 {
		t.Errorf("request count = %d, want 2", atomic.LoadInt32(&rt.count))
	}
}

func TestChatStreamRetryDisabled(t *testing.T) {
	rt := &countRT{fn: func(i int) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Status: "503", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`"msg":"boom"`))}, nil
	}}
	c := testRetryClient(t, rt, RetryConfig{Enabled: false, Attempts: 3, Backoff: time.Millisecond})
	_, _, _, err := c.ChatStreamContext(context.Background(), newRetryTestAuth(), []byte(`{}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if atomic.LoadInt32(&rt.count) != 1 {
		t.Errorf("request count = %d, want 1 (retry disabled)", atomic.LoadInt32(&rt.count))
	}
}

func TestRetryAllowed(t *testing.T) {
	cases := []struct {
		kind  ErrKind
		trans bool
		want  bool
	}{
		{ErrServer, false, true},
		{ErrNone, true, true}, // 传输层错误（kind 未定）
		{ErrSoftRate, false, false},
		{ErrHardCredit, false, false},
		{ErrSessionDead, false, false},
		{ErrAccountFault, false, false},
		{ErrWafBlock, false, false},
		{ErrNotFound, false, false},
		{ErrContentBlocked, false, false},
		{ErrBadParams, false, false},
		{ErrModelBlocked, false, false},
		{ErrPromptTooLong, false, false},
		{ErrImageInvalid, false, false},
		{ErrToolCallMismatch, false, false},
		{ErrClient, false, false},
	}
	for _, tc := range cases {
		if got := retryAllowed(tc.kind, tc.trans); got != tc.want {
			t.Errorf("retryAllowed(%v, %v) = %v, want %v", tc.kind, tc.trans, got, tc.want)
		}
	}
}
