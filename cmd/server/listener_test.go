package main

import (
	"net"
	"net/http"
	"testing"
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
