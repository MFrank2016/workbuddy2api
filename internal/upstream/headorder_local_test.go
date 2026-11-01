package upstream

import (
	"net"
	"strings"
	"testing"
)

// scrambledChatHead 故意按与官方相反的顺序写：官方顺序见 officialHeadOrder。
const scrambledChatHead = "POST /v2/chat/completions HTTP/1.1\r\n" +
	"Authorization: Bearer at\r\n" +
	"User-Agent: WorkBuddy/5.9.3\r\n" +
	"X-Request-ID: r1\r\n" +
	"X-Conversation-ID: c1\r\n" +
	"Accept: application/json\r\n" +
	"\r\n"

func assertOrder(t *testing.T, got string, names ...string) {
	t.Helper()
	prev := -1
	for _, n := range names {
		i := strings.Index(got, n+":")
		if i < 0 {
			t.Fatalf("missing header %q in:\n%s", n, got)
		}
		if i <= prev {
			t.Fatalf("%q out of order in:\n%s", n, got)
		}
		prev = i
	}
}

// TestReorderHeadBlock 按官方 chat 顺序重排：Accept → X-Conversation-ID →
// X-Request-ID → User-Agent → Authorization（官方真机顺序的子序列）。
func TestReorderHeadBlock(t *testing.T) {
	out := string(reorderHeadBlock([]byte(scrambledChatHead)))
	assertOrder(t, out, "Accept", "X-Conversation-ID", "X-Request-ID", "User-Agent", "Authorization")
	// 终止空行必须保留（否则请求非法）。
	if !strings.HasSuffix(out, "\r\n\r\n") {
		t.Fatalf("head block must end with CRLFCRLF: %q", out)
	}
}

// TestReorderHeadBlockUnknownPath 顺序表外的路径原样返回（不做无证据的猜测）。
func TestReorderHeadBlockUnknownPath(t *testing.T) {
	in := strings.Replace(scrambledChatHead, "/v2/chat/completions", "/v9/unknown", 1)
	if got := string(reorderHeadBlock([]byte(in))); got != in {
		t.Fatalf("unknown path must pass through unchanged:\n%s", got)
	}
}

// TestHeadOrderConnReordersWrite conn 层重排：写入被打乱的请求头，对端读到的
// 必须是官方顺序。
func TestHeadOrderConnReordersWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	hc := newHeadOrderConn(client)
	go func() { _, _ = hc.Write([]byte(scrambledChatHead)) }()

	buf := make([]byte, 8192)
	n, err := server.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertOrder(t, string(buf[:n]), "Accept", "X-Conversation-ID", "X-Request-ID", "User-Agent", "Authorization")
}

// TestHeadOrderSwitchDefaultOff 默认不包装（行为与改前一致）。
func TestHeadOrderSwitchDefaultOff(t *testing.T) {
	if headOrderEnabled.Load() {
		t.Fatal("head order must default to off")
	}
}
