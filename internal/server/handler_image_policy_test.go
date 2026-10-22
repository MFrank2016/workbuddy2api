// handler_image_policy_test.go 图片策略（upstream.max_images / image_policy）端到端验收：
//   - flag off（默认 0/passthrough）出站 body 与 W0 golden 逐字一致；
//   - truncate 保留最新 N 张（17 -> 16，丢最旧）；
//   - error 超限 413 too_many_images（image_count / max_images / param="messages"）；
//   - 未超限透传；flag off 时图片一张不少。
//
// 能力预检（capability_guard）的端到端验收在 handler_capability_test.go：
// 两条链路共用同一 fake 捕获器（chatCapture / captureUpstream / imagesChatBody）。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// chatCapture 捕获 handler 出站 chat 请求体（fake 上游零网络）。
type chatCapture struct {
	mu   sync.Mutex
	body []byte
	hits int
}

func (c *chatCapture) last() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

func (c *chatCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// captureUpstream 构造只回 SSE 200 的 fake 客户端，并记录最后一次出站 body。
func captureUpstream(t *testing.T, cap *chatCapture) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.body = raw
			cap.hits++
			cap.mu.Unlock()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
}

// imagesChatBody 构造带 n 张图片（URL 唯一标记 tag-i）的 chat 请求体。
func imagesChatBody(model string, n int, tag string) string {
	parts := make([]string, 0, n+1)
	parts = append(parts, `{"type":"text","text":"look"}`)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf(`{"type":"image_url","image_url":{"url":"https://example.test/%s-%d.png"}}`, tag, i))
	}
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[%s]}]}`, model, strings.Join(parts, ","))
}

// TestChatFlagOffPayloadMatchesGolden 默认配置（max_images=0 / passthrough /
// capability_guard=false）下，handler 出站 chat body 与 W0 golden 逐字一致。
//
// 复现 golden 的 prompt_cache_key：golden 采集时 uid=golden-uid-0001 +
// conversationID=golden-conversation；handler 侧不传会话 ID（body 里没有
// conversation_id），故这里在入站 body 显式带 golden 的 key ——
// InjectPromptCacheKey 对显式 key 绝不覆盖，出站体因此与 golden 完全相同。
func TestChatFlagOffPayloadMatchesGolden(t *testing.T) {
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "golden-uid-0001", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	inbound := `{"model":"glm-5.2","messages":[{"role":"system","content":"You are a careful assistant."},` +
		`{"role":"user","content":"Summarize the migration plan in one line."}],` +
		`"max_tokens":256,"temperature":0.2,` +
		`"prompt_cache_key":"wb2a-golden-u-8c7223272dc7a6fe829d68372a21d4f1"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(inbound)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	got := cap.last()
	if len(got) == 0 {
		t.Fatal("no outbound body captured")
	}
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("outbound not json: %v (%s)", err, got)
	}
	pretty, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatalf("indent: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("..", "upstream", "testdata", "golden", "chat_request_body.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(want) != string(pretty)+"\n" {
		t.Errorf("flag-off 出站 body 与 golden 不一致\n--- want ---\n%s\n--- got ---\n%s\n", want, pretty)
	}
}

// TestChatImagePolicyTruncate 17 张 + max_images=16 + truncate → 出站 16 张，
// 保留最新 16 张（丢弃 img-0），请求正常打到上游。
func TestChatImagePolicyTruncate(t *testing.T) {
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxImages: 16, ImagePolicy: "truncate"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imagesChatBody("glm-5.2", 17, "img"))))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	got := cap.last()
	if n := upstream.CountImages(got); n != 16 {
		t.Errorf("出站图片数=%d want 16", n)
	}
	if bytes.Contains(got, []byte("img-0.png")) {
		t.Error("最旧的 img-0 必须被丢弃")
	}
	if !bytes.Contains(got, []byte("img-16.png")) {
		t.Error("最新的 img-16 必须保留")
	}
}

// TestChatImagePolicyError413 17 张 + max_images=16 + error → 413 too_many_images，
// error 对象含 image_count=17 / max_images=16 / param="messages"；零上游请求。
func TestChatImagePolicyError413(t *testing.T) {
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxImages: 16, ImagePolicy: "error"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imagesChatBody("glm-5.2", 17, "img"))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code       string `json:"code"`
			Type       string `json:"type"`
			Param      string `json:"param"`
			ImageCount int    `json:"image_count"`
			MaxImages  int    `json:"max_images"`
			Message    string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if env.Error.Code != "too_many_images" {
		t.Errorf("code=%q want too_many_images", env.Error.Code)
	}
	if env.Error.ImageCount != 17 || env.Error.MaxImages != 16 {
		t.Errorf("image_count=%d max_images=%d want 17/16", env.Error.ImageCount, env.Error.MaxImages)
	}
	if env.Error.Param != "messages" {
		t.Errorf("param=%q want messages", env.Error.Param)
	}
	if cap.count() != 0 {
		t.Errorf("超限必须零上游请求，got %d", cap.count())
	}
}

// TestChatImagePolicyErrorUnderLimit 未超限 → 正常透传（error 策略只在超限时生效）。
func TestChatImagePolicyErrorUnderLimit(t *testing.T) {
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxImages: 16, ImagePolicy: "error"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imagesChatBody("glm-5.2", 3, "img"))))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if n := upstream.CountImages(cap.last()); n != 3 {
		t.Errorf("出站图片数=%d want 3", n)
	}
}
