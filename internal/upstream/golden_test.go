// golden_test.go 预迁移 golden：出站 chat 请求体 + URL + 头集合（W0 基线）。
//
// 为什么用 golden 而不是字段断言
// ------------------------------
// 迁移前后要证明「flag 全关时行为与迁移前逐字一致」。字段断言只覆盖作者想到的
// 字段，多出来的字段（比如某次改动顺手加的 stream_options 子键）不会让它失败，
// 而客户端看到的东西已经变了。所以这里把**真正发往上游的字节**整体落成 golden。
//
// 采集方式
// --------
// 走真实代码路径：`Client.ChatStream`（含 prepareBody 的 stream/stream_options/
// 模型归一/prompt_cache_key 注入等全部改写）+ 真实 `ChatHeaders`，只把
// RoundTripper 换成捕获器。没有测试专用接缝——transport 本来就是可注入字段。
//
// 归一化（写盘前统一处理，见 golden/README.md）
// -------------------------------------------
//   - JSON：`json.MarshalIndent` + 键排序（encoding/json 对 map 天然按键排序）；
//   - 头集合：按键排序后写成 `name: value` 行；
//   - 消息级 ID（X-Conversation-Message-ID / X-Request-ID / X-B3-SpanId）由
//     `session.NewMessageID()` 每次新生成，属于天生不确定值 → 统一替换成占位符。
//     会话级 ID（X-Conversation-Request-ID 等）由测试传入固定 ChatMeta，稳定。
//
// 重新生成
// --------
//
//	WB2A_UPDATE_GOLDEN=1 go test ./internal/upstream/ -run 'TestGolden'
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

const (
	goldenDir            = "testdata/golden"
	goldenChatBodyFile   = "chat_request_body.json"
	goldenChatTargetFile = "chat_url_headers.txt"
)

// goldenChatInbound 入站 chat 请求（合成 fixture；字段覆盖迁移会碰到的形状）。
const goldenChatInbound = `{"model":"glm-5.2","messages":[{"role":"system","content":"You are a careful assistant."},{"role":"user","content":"Summarize the migration plan in one line."}],"max_tokens":256,"temperature":0.2}`

// goldenAuth 固定账号：UID/企业 ID/域全部写死，派生头（X-Machine-ID /
// X-Session-ID）因此稳定；AccessToken 是合成值，不是任何真实凭据。
func goldenAuth() *auth.Auth {
	return &auth.Auth{
		AccessToken:  "golden-access-token",
		UID:          "golden-uid-0001",
		EnterpriseID: "golden-enterprise",
		Domain:       "copilot.tencent.com",
		ExpiresAt:    9999999999,
	}
}

// goldenMeta 固定会话头族元数据（不传就让 ChatHeaders 现生成随机 ID）。
func goldenMeta() ChatMeta {
	return ChatMeta{
		ConversationID:        "golden-conversation",
		ConversationHeaderID:  "golden-conversation",
		ConversationRequestID: "0123456789abcdef0123456789abcdef",
		TraceID:               "0123456789abcdef0123456789abcdef",
	}
}

// goldenChatTarget 一次出站捕获的结果。
type goldenChatTarget struct {
	body []byte
	url  string
	head http.Header
}

// captureGoldenChat 走真实 ChatStream 捕获出站请求（body/url/headers）。
func captureGoldenChat(t *testing.T) goldenChatTarget {
	t.Helper()
	var got goldenChatTarget
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			got.url = r.URL.String()
			got.head = r.Header.Clone()
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read outbound body: %v", err)
			}
			got.body = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	rc, status, _, err := c.ChatStream(goldenAuth(), []byte(goldenChatInbound), "", goldenMeta())
	if err != nil || status != 200 {
		t.Fatalf("ChatStream: status=%d err=%v", status, err)
	}
	rc.Close()
	return got
}

// normalizeGoldenHeaders 把每次都会变的消息级 ID 换成占位符（其余原样保留）。
//
// 只打码这三个：它们来自 session.NewMessageID()（crypto/rand），是本次请求的
// 消息标识，不属于「协议形状」。会话级 ID 由测试固定，所以保持原值——
// 否则 golden 就证明不了「会话头族确实发出去了」。
func normalizeGoldenHeaders(h http.Header) http.Header {
	out := h.Clone()
	for _, name := range []string{"X-Conversation-Message-Id", "X-Request-Id", "X-B3-Spanid"} {
		if out.Get(name) != "" {
			out.Set(name, "<message-id>")
		}
	}
	return out
}

// renderGoldenHeaders 渲染成 `name: value` 的排序行文本（键小写、值原样）。
func renderGoldenHeaders(h http.Header) string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		values := h[name]
		for _, value := range values {
			b.WriteString(strings.ToLower(name))
			b.WriteString(": ")
			b.WriteString(value)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderGoldenBody 归一化 JSON 请求体：缩进 + 键排序 + 末尾换行。
func renderGoldenBody(t *testing.T, raw []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("outbound body is not JSON: %v (%s)", err, raw)
	}
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	return string(out) + "\n"
}

// writeOrCompareGolden 更新模式写盘，否则逐字节比对。
func writeOrCompareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	if os.Getenv("WB2A_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with WB2A_UPDATE_GOLDEN=1)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s 与 golden 不一致 —— 若本次改动是有意的，用 WB2A_UPDATE_GOLDEN=1 重新生成并写清差异\n--- want ---\n%s\n--- got ---\n%s",
			name, want, got)
	}
}

// TestGoldenChatRequestBody 出站 chat 请求体逐字节快照。
//
// 覆盖的真实改写：`stream` 强制 true、`stream_options.include_usage` 补入、
// `prompt_cache_key` 按账号+会话注入、model 原样（glm 不是 deepseek，不注入
// thinking）。这些都是「上游只认这个形状」的既有约定。
func TestGoldenChatRequestBody(t *testing.T) {
	got := captureGoldenChat(t)
	writeOrCompareGolden(t, goldenChatBodyFile, renderGoldenBody(t, got.body))

	// 形状自证：golden 若被误生成为别的协议，下面几条会先失败。
	var obj map[string]any
	if err := json.Unmarshal(got.body, &obj); err != nil {
		t.Fatalf("unmarshal outbound: %v", err)
	}
	if obj["stream"] != true {
		t.Errorf("stream=%v want true (ChatStream 必须强制流式)", obj["stream"])
	}
	if obj["model"] != "glm-5.2" {
		t.Errorf("model=%v want glm-5.2", obj["model"])
	}
	if _, ok := obj["prompt_cache_key"]; !ok {
		t.Error("prompt_cache_key 缺失（P0 费用优化的注入点）")
	}
}

// TestGoldenChatURLAndHeaders 出站 URL 与头集合快照。
//
// URL 决定打的是哪个上游端点（/v2/chat/completions）；头集合决定上游是否把
// 请求当成官方客户端（风控闸门头、指纹头、会话头族）。两者都不是「字段断言」
// 能覆盖的：多一个头、少一个头，客户端与上游看到的就不一样了。
func TestGoldenChatURLAndHeaders(t *testing.T) {
	got := captureGoldenChat(t)
	render := "url: " + got.url + "\n" + renderGoldenHeaders(normalizeGoldenHeaders(got.head))
	writeOrCompareGolden(t, goldenChatTargetFile, render)

	if got.url != "https://chat.example/v2/chat/completions" {
		t.Errorf("url=%q want https://chat.example/v2/chat/completions", got.url)
	}
	for _, name := range []string{
		"Authorization", "X-CodeBuddy-Request", "X-Conversation-Request-Id",
		"X-Machine-Id", "X-Session-Id", "User-Agent", "Origin", "Referer",
	} {
		if got.head.Get(name) == "" {
			t.Errorf("缺少出站头 %s（上游风控/会话聚合依赖它）", name)
		}
	}
}

// TestGoldenCaptureIsDeterministic 两次采集必须完全相同。
//
// golden 的价值全在「逐字可复现」：若采集本身带随机性，比对就永远在红与绿之间
// 抖动，而抖动会被当成环境噪音忽略掉 —— 那 golden 就白做了。
func TestGoldenCaptureIsDeterministic(t *testing.T) {
	first := captureGoldenChat(t)
	second := captureGoldenChat(t)
	if !bytes.Equal(first.body, second.body) {
		t.Errorf("两次采集的请求体不同:\n%s\n---\n%s", first.body, second.body)
	}
	if first.url != second.url {
		t.Errorf("两次采集的 URL 不同: %q vs %q", first.url, second.url)
	}
	firstHead := renderGoldenHeaders(normalizeGoldenHeaders(first.head))
	secondHead := renderGoldenHeaders(normalizeGoldenHeaders(second.head))
	if firstHead != secondHead {
		t.Errorf("两次采集的（已归一化）头集合不同:\n%s\n---\n%s", firstHead, secondHead)
	}
}
