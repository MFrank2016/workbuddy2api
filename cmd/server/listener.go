package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// ListenerManager 管理 core 的单 listen 地址热切换。
//
// Apply 先绑定新地址，成功后才关闭旧 listener；旧 server 已接受的连接继续完成
// （Serve 返回后已接受的连接仍由各自 goroutine 处理），不会被主动断开。
type ListenerManager struct {
	mu        sync.Mutex
	newServer func(addr string) *http.Server
	srv       *http.Server
	ln        net.Listener
	addr      string
}

func NewListenerManager(newServer func(addr string) *http.Server) *ListenerManager {
	return &ListenerManager{newServer: newServer}
}

// Start 绑定并启动初始监听。
func (m *ListenerManager) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := m.newServer(addr)
	m.mu.Lock()
	m.ln, m.srv, m.addr = ln, srv, ln.Addr().String()
	m.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			// 服务异常只记录；监听生命周期由 Apply/Close 管理。
			fmt.Printf("[listener] serve %s: %v\n", addr, err)
		}
	}()
	return nil
}

// Apply 热切换监听地址：新地址 bind 成功后才关闭旧 listener。
func (m *ListenerManager) Apply(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := m.newServer(addr)
	m.mu.Lock()
	oldLn := m.ln
	m.ln, m.srv, m.addr = ln, srv, ln.Addr().String()
	m.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			fmt.Printf("[listener] serve %s: %v\n", addr, err)
		}
	}()
	if oldLn != nil {
		_ = oldLn.Close()
	}
	return nil
}

// Addr 返回当前实际监听地址。
func (m *ListenerManager) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addr
}

// shutdownGrace 优雅停机时等待在途请求结束的上限。
//
// 为什么不是 5s：SSE 流的首字延迟实测 10–40s、整条流 10–45s，5s 会把正在
// 生成的回答从中间掐断（客户端看到 ECONNRESET / incomplete chunked read）。
// 这里给到 120s，覆盖绝大多数流；docker-compose 的 stop_grace_period 要比
// 它再宽一点（见 docker-compose.yml），留出 p.Flush()/store.Close() 的时间。
var shutdownGrace = 120 * time.Second

// Close 关闭当前 listener 并优雅停机。
func (m *ListenerManager) Close() error {
	m.mu.Lock()
	ln, srv := m.ln, m.srv
	m.ln, m.srv = nil, nil
	m.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(ctx)
	}
	return nil
}

// sameListen 比较两个监听地址是否等价（端口相同 + 主机归一化）。
// ":7863" / "0.0.0.0:7863" / "[::]:7863" 视为同一个通配监听。
func sameListen(actual, want string) bool {
	ah, ap, e1 := net.SplitHostPort(actual)
	wh, wp, e2 := net.SplitHostPort(want)
	if e1 != nil || e2 != nil || ap != wp {
		return false
	}
	norm := func(h string) string {
		if h == "" || h == "::" || h == "0.0.0.0" {
			return ""
		}
		return h
	}
	return norm(ah) == norm(wh)
}
