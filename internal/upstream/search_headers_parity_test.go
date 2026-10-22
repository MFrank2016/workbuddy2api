package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// conversationHeaderFamilyKeys 会话/链路身份头族的显式最小集合（上游渠道校验口径）：
// X-Conversation-ID 缺头会被判非官方客户端并 400 code=11-128，其余为同族聚合/链路头。
// 搜索与真实 chat 出站必须同族，否则搜索请求在渠道侧与 chat 身份不一致。
var conversationHeaderFamilyKeys = []string{
	"X-Conversation-Id",
	"X-Conversation-Request-Id",
	"X-Conversation-Message-Id",
	"X-Request-Id",
	"X-Root-Request-Id",
	"X-Trace-Id",
	"X-B3-Traceid",
	"X-B3-Spanid",
	"X-B3-Sampled",
}

// isConversationHeaderFamilyKey 判定某头是否属于会话/链路身份头族（前缀 + 精确名）：
// 用于把 chat 侧头集里属于该族的键整体取出，断言搜索侧是它的超集（未来新增族内头
// 也会被自动纳入，无需改测试）。
func isConversationHeaderFamilyKey(k string) bool {
	switch k {
	case "X-Request-Id", "X-Root-Request-Id", "X-Trace-Id":
		return true
	}
	return strings.HasPrefix(k, "X-Conversation-") || strings.HasPrefix(k, "X-B3-")
}

// TestSearchConversationHeadersParityWithChat 搜索出站必须携带与真实 chat 出站同族的
// 会话/身份头（superset），且值非空：handler 的真实 chat 路径恒发该族（X-Conversation-ID
// 由轮级聚合键兜底，上游渠道校验缺头即 400 code=11-128），参考实现 a10bb17 的
// _web_search 同样带 X-Conversation-ID。零值 ChatMeta 会让 X-Conversation-ID 缺失。
func TestSearchConversationHeadersParityWithChat(t *testing.T) {
	chatHeaders := make(chan http.Header, 1)
	searchHeaders := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case chatCompletionsPath:
			chatHeaders <- r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {}\n\n"))
		case searchPath:
			searchHeaders <- r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(searchTwoResults))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	// 真实 chat 路径：与 handler.go 一致——轮级聚合键 32 hex，无客户端 conversationId
	// 时用同一聚合键兜底 ConversationHeaderID（X-Conversation-ID 恒发）。
	convReqID := "0123456789abcdef0123456789abcdef"
	meta := ChatMeta{ConversationRequestID: convReqID, ConversationHeaderID: convReqID}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, body, "", meta)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_ = rc.Close()
	if status != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", status)
	}

	if _, err := c.Search(a, "q", 1); err != nil {
		t.Fatalf("search: %v", err)
	}

	var chat http.Header
	select {
	case chat = <-chatHeaders:
	case <-time.After(5 * time.Second):
		t.Fatal("no chat request observed")
	}
	var search http.Header
	select {
	case search = <-searchHeaders:
	case <-time.After(5 * time.Second):
		t.Fatal("no search request observed")
	}

	// 1) 显式最小集合：搜索侧每个头族键必须存在且非空（含 X-Conversation-ID）。
	for _, k := range conversationHeaderFamilyKeys {
		if got := search.Get(k); got == "" {
			t.Errorf("search missing %s (chat has %q)", k, chat.Get(k))
		}
	}
	// 2) 超集：chat 侧族内任意头（含未来新增）搜索侧都必须有非空值。
	for k, v := range chat {
		if !isConversationHeaderFamilyKey(k) {
			continue
		}
		if v[0] == "" {
			continue // chat 自身未发非空值，不构成要求
		}
		if got := search.Get(k); got == "" {
			t.Errorf("search header set not superset of chat for %s: chat=%q search=%q", k, v[0], got)
		}
	}
	// 3) 搜索单次请求内值稳定且合法：X-Conversation-ID == X-Conversation-Request-ID
	//    （与 handler 的轮级兜底语义一致），B3 TraceId 为 32 hex。
	if conv, req := search.Get("X-Conversation-Id"), search.Get("X-Conversation-Request-Id"); conv != req {
		t.Errorf("search X-Conversation-ID = %q, X-Conversation-Request-ID = %q; want same synthesized turn key", conv, req)
	}
	if b3 := search.Get("X-B3-TraceId"); !hex32Re.MatchString(b3) {
		t.Errorf("search X-B3-TraceId = %q, want 32 hex", b3)
	}
}
