package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// ReplayStream 包装上游流，在「客户端尚未收到任何数据」且首次 Read 返回
// 传输层瞬时错误时重开一次。首字节后、客户端取消后、非瞬时错误一律不重放。
type ReplayStream struct {
	Ctx      context.Context
	RC       io.ReadCloser
	Reopen   func(context.Context) (io.ReadCloser, error)
	Attempts int
	sent     bool
}

// NewReplayStream 构造首字节重放包装。ctx 为 nil 时退化为 Background。
func NewReplayStream(ctx context.Context, rc io.ReadCloser, reopen func(context.Context) (io.ReadCloser, error)) *ReplayStream {
	if ctx == nil {
		ctx = context.Background()
	}
	return &ReplayStream{Ctx: ctx, RC: rc, Reopen: reopen}
}

// Read 读取底流；仅当本次读取未产出任何字节、错误为瞬时网络错误、
// 尚未重放过且客户端 ctx 未取消时，关闭旧流并重开一次。
func (s *ReplayStream) Read(p []byte) (int, error) {
	n, err := s.RC.Read(p)
	if n > 0 {
		s.sent = true
	}
	if err == nil || s.sent || s.Attempts >= 1 || s.Reopen == nil || !isTransientStreamError(err) {
		return n, err
	}
	if s.Ctx != nil && s.Ctx.Err() != nil {
		return n, err
	}
	s.Attempts++
	_ = s.RC.Close()
	rc, oerr := s.Reopen(s.Ctx)
	if oerr != nil || rc == nil {
		return n, err
	}
	s.RC = rc
	return s.Read(p)
}

// Close 关闭当前底流。
func (s *ReplayStream) Close() error {
	if s.RC == nil {
		return nil
	}
	return s.RC.Close()
}

// isTransientStreamError 判定可重放的传输层瞬时错误：连接重置 / 断管 /
// 非预期 EOF / 连接已关闭 / HTTP2 GOAWAY。客户端取消与 deadline 明确排除。
func isTransientStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"connection reset", "broken pipe", "unexpected eof", "goaway", "go away"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
