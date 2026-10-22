// handler_capability_test.go 模型能力预检（upstream.capability_guard）端到端验收：
//   - 显式 supportsImages=false + 带图 + 预检开 → 400 model_capability_mismatch
//     （读 body 后、选号前：零上游 chat 请求）；
//   - 能力缺失（tri-state Set=false）→ 放行（fail-open），hint 不声称不支持；
//   - 预检默认关（不得默认开启）；
//   - global 模型经 realm-aware 目录同样命中。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestChatCapabilityGuardExplicitFalse 显式 supportsImages=false + 带图 + 预检开
// → 400 model_capability_mismatch，零上游 chat 请求（读 body 后、选号前）。
func TestChatCapabilityGuardExplicitFalse(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v3-0324", SupportsImagesSet: true, SupportsImages: false},
	})
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, CapabilityGuard: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(imagesChatBody("deepseek-v3-0324", 1, "img"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code    string  `json:"code"`
			Message string  `json:"message"`
			Hint    *string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if env.Error.Code != "model_capability_mismatch" {
		t.Errorf("code=%q want model_capability_mismatch", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "deepseek-v3-0324") {
		t.Errorf("message 应点名模型: %q", env.Error.Message)
	}
	if env.Error.Hint == nil || !strings.Contains(*env.Error.Hint, "does not support images") {
		t.Errorf("gateway_hint=%v want model-not-supports-images hint", env.Error.Hint)
	}
	if cap.count() != 0 {
		t.Errorf("预检必须发生在选号前：上游 chat 请求数=%d want 0", cap.count())
	}
}

// TestChatCapabilityGuardMissingCapabilityFailOpen 目录收录但 supportsImages
// **缺失**（Set=false）→ 放行（fail-open），不判不支持。
func TestChatCapabilityGuardMissingCapabilityFailOpen(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "kimi-k2.7"}}) // Set=false：能力未知
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, CapabilityGuard: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(imagesChatBody("kimi-k2.7", 1, "img"))))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200（能力缺失必须 fail-open）body=%s", rec.Code, rec.Body)
	}
	if cap.count() != 1 {
		t.Errorf("上游 chat 请求数=%d want 1", cap.count())
	}
}

// TestChatCapabilityGuardOffIgnoresExplicitFalse 预检关闭（默认）时即便目录显式
// 不支持也不拦（不得默认开启）。
func TestChatCapabilityGuardOffIgnoresExplicitFalse(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v3-0324", SupportsImagesSet: true, SupportsImages: false},
	})
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(imagesChatBody("deepseek-v3-0324", 1, "img"))))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200（capability_guard 默认关）body=%s", rec.Code, rec.Body)
	}
}

// globalCatalogClient 构造能同时服务 global 模型目录探测与 chat 的 fake 客户端：
// GET → 显式 supportsImages=false 的 global 目录；POST chat → SSE 200。
// 返回客户端与 chat 捕获器（用于断言预检发生在选号前）。
func globalCatalogClient(t *testing.T) (*upstream.Client, *chatCapture) {
	t.Helper()
	cap := &chatCapture{}
	cl := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"code":0,"data":{"models":[{"id":"gpt-5.4","name":"GPT-5.4","supportsImages":false}]}}`)),
				}, nil
			}
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
		ChatBaseCN:     "https://fake.example",
		ChatBaseGlobal: "https://fake.example",
		GlobalEnabled:  true,
	}
	return cl, cap
}

// TestChatCapabilityGuardGlobalModel global 模型（realm=global）同样命中预检：
// 目录来自 global 探测缓存（CN 缓存为空也不影响）。
func TestChatCapabilityGuardGlobalModel(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	defer resetModelsCache()

	cl, cap := globalCatalogClient(t)
	globalAcct := &auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	// 预热 global 目录缓存（与 /v1/models 的探测同源；预检本身零上游调用）。
	if infos := cl.FetchGlobalModelInfos(globalAcct); len(infos) != 1 || infos[0].ID != "gpt-5.4" {
		t.Fatalf("warm global catalog: %+v", infos)
	}
	p := testPoolWith(globalAcct)
	h := NewHandler(Config{Pool: p, Upstream: cl, CapabilityGuard: true, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(imagesChatBody("global:gpt-5.4", 1, "img"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（global 模型也须命中预检）body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_capability_mismatch") {
		t.Errorf("code missing: %s", rec.Body)
	}
	if cap.count() != 0 {
		t.Errorf("预检必须在选号前：chat 请求数=%d want 0", cap.count())
	}
}

// TestChatHintMissingCapabilityNeutral 11133 + 带图 + 目录收录但能力缺失
// （Set=false）→ hint 不得声称「不支持图片」（与预检同一 fail-open 口径）。
func TestChatHintMissingCapabilityNeutral(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "deepseek-v3-0324"}})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11133Real, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if e.Error.GatewayHint == nil {
		t.Fatal("11133 must carry a hint")
	}
	if strings.Contains(*e.Error.GatewayHint, "does not support images") {
		t.Errorf("缺失能力不得声称不支持: %q", *e.Error.GatewayHint)
	}
}

// TestChatImagePolicyFlagOffBytesIdentical 关闭态（默认零值 Config）出站 body
// 与入站 body 的语义字段完全一致（图片一张不少）——flag off 不碰 payload。
func TestChatImagePolicyFlagOffBytesIdentical(t *testing.T) {
	cap := &chatCapture{}
	up := captureUpstream(t, cap)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imagesChatBody("glm-5.2", 17, "img"))))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if n := upstream.CountImages(cap.last()); n != 17 {
		t.Errorf("关闭态出站图片数=%d want 17（不得改写）", n)
	}
}

// mixedImageChatBody 构造「混合 content 形态」的请求体：system 用**字符串** content
// （OpenAI 客户端最常见的形状），user 用 content 数组携带一张 image_url。
// 这是 A4(f) 缺陷的最小真实复现：字符串形态曾让单类型解码整包失败，
// 预检与 11133 image hint 双双静默失效。
func mixedImageChatBody(model string) string {
	return `{"model":` + strconv.Quote(model) + `,"messages":[` +
		`{"role":"system","content":"You are a careful assistant."},` +
		`{"role":"user","content":[{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"https://example.test/mixed.png"}}]}]}`
}

// toolResultImageChatBody 图片只出现在 tool 结果里（user 是纯字符串）：
// tool 结果的 content 同样是 content 数组，计数与预检口径都不分 role。
func toolResultImageChatBody(model string) string {
	return `{"model":` + strconv.Quote(model) + `,"messages":[` +
		`{"role":"user","content":"read that screenshot"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function",` +
		`"function":{"name":"read_file","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"here"},` +
		`{"type":"image_url","image_url":"https://example.test/tool.png"}]}]}`
}

// TestChatCapabilityGuardMixedContentShapes 字符串 / null / 缺失 content 不得掩盖
// 其他消息里的图片：显式 supportsImages=false + 带图 + 预检开 → 400，且零上游 chat。
func TestChatCapabilityGuardMixedContentShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"system string + user array", mixedImageChatBody("deepseek-v3-0324")},
		{"tool-result-only image", toolResultImageChatBody("deepseek-v3-0324")},
		{"null content before image", `{"model":"deepseek-v3-0324","messages":[` +
			`{"role":"assistant","content":null},` +
			`{"role":"system"},` +
			`{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/x.png"}]}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetModelsCache()
			defer resetModelsCache()
			seedModelsCache([]upstream.ModelInfo{
				{ID: "deepseek-v3-0324", SupportsImagesSet: true, SupportsImages: false},
			})
			cap := &chatCapture{}
			h := NewHandler(Config{
				Pool:            testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:        captureUpstream(t, cap),
				CapabilityGuard: true,
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400（混合 content 形态不得让预检 fail-open）body=%s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), "model_capability_mismatch") {
				t.Errorf("code missing: %s", rec.Body)
			}
			if cap.count() != 0 {
				t.Errorf("预检必须在选号前：chat 请求数=%d want 0", cap.count())
			}
		})
	}
}

// TestChatCapabilityGuardMixedContentAbsentCapabilityAllows 同一混合 body，但目录
// 未声明能力（Set=false）→ fail-open 放行（缺失 ≠ 不支持）。
func TestChatCapabilityGuardMixedContentAbsentCapabilityAllows(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "deepseek-v3-0324"}}) // Set=false
	cap := &chatCapture{}
	h := NewHandler(Config{
		Pool:            testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:        captureUpstream(t, cap),
		CapabilityGuard: true,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(mixedImageChatBody("deepseek-v3-0324"))))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200（能力缺失必须 fail-open）body=%s", rec.Code, rec.Body)
	}
	if cap.count() != 1 {
		t.Errorf("上游 chat 请求数=%d want 1", cap.count())
	}
}
