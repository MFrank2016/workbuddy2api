package upstream

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type errReadCloser struct{ err error }

func (r *errReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *errReadCloser) Close() error             { return nil }

type dataThenErr struct {
	data []byte
	err  error
	done bool
}

func (r *dataThenErr) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, r.err
}
func (r *dataThenErr) Close() error { return nil }

func TestReplayOnFirstReadError(t *testing.T) {
	first := &errReadCloser{err: io.ErrUnexpectedEOF}
	second := io.NopCloser(strings.NewReader("ok"))
	reopens := 0
	rs := NewReplayStream(context.Background(), first, func(context.Context) (io.ReadCloser, error) {
		reopens++
		return second, nil
	})
	buf := make([]byte, 2)
	n, err := rs.Read(buf)
	if err != nil || string(buf[:n]) != "ok" {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if reopens != 1 {
		t.Fatalf("reopens=%d", reopens)
	}
	if rs.Attempts != 1 {
		t.Fatalf("attempts=%d want 1", rs.Attempts)
	}
}

func TestNoReplayAfterData(t *testing.T) {
	first := &dataThenErr{data: []byte("x"), err: io.ErrUnexpectedEOF}
	rs := NewReplayStream(context.Background(), first, func(context.Context) (io.ReadCloser, error) {
		t.Fatal("must not reopen after data")
		return nil, nil
	})
	buf := make([]byte, 1)
	if n, _ := rs.Read(buf); n != 1 {
		t.Fatalf("first read n=%d want 1", n)
	}
	if _, err := rs.Read(buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("second read err=%v want unexpected EOF", err)
	}
}

func TestReplayContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reopens := 0
	rs := NewReplayStream(ctx, &errReadCloser{err: io.ErrUnexpectedEOF}, func(context.Context) (io.ReadCloser, error) {
		reopens++
		return io.NopCloser(strings.NewReader("ok")), nil
	})
	buf := make([]byte, 2)
	if _, err := rs.Read(buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v want unexpected EOF", err)
	}
	if reopens != 0 {
		t.Fatalf("reopens=%d want 0 after ctx cancel", reopens)
	}
}

func TestNoReplayNonTransient(t *testing.T) {
	reopens := 0
	rs := NewReplayStream(context.Background(), &errReadCloser{err: errors.New("boom")}, func(context.Context) (io.ReadCloser, error) {
		reopens++
		return io.NopCloser(strings.NewReader("ok")), nil
	})
	buf := make([]byte, 2)
	if _, err := rs.Read(buf); err == nil || err.Error() != "boom" {
		t.Fatalf("err=%v want boom", err)
	}
	if reopens != 0 {
		t.Fatalf("reopens=%d want 0 for non-transient error", reopens)
	}
}
