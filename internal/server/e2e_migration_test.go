// e2e_migration_test.go Todo 33：跨能力端到端（untagged、httptest、fake pool）。
//
// 不读 ../auths、不 skip：每个子用例都走真实 Handler.ServeHTTP → 选号 →
// upstream.Client（fake transport）→ 响应。
package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func e2eUpstream(t *testing.T, urls *[]string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*urls = append(*urls, r.URL.String())
			if strings.Contains(r.URL.Path, "/agenttool/v1/search") {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"code":0,"results":[{"title":"T","url":"https://e.x","snippet":"S"}]}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n\n" +
						`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n" +
						"data: [DONE]\n\n")),
			}, nil
		})},
	}
}

func e2eAccount(profile string) *auth.Auth {
	return &auth.Auth{
		UID:         "u-" + profile,
		AccessToken: "at-" + profile,
		Domain:      map[string]string{"cn-work": "www.workbuddy.cn", "cn-cli": "copilot.tencent.com"}[profile],
		Profile:     profile,
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}
}

func TestE2EMigration(t *testing.T) {
	t.Run("profile_routing", func(t *testing.T) {
		var urls []string
		up := e2eUpstream(t, &urls)
		up.ProfilesEnabled = true
		h := NewHandler(Config{
			Pool:     testPoolWith(e2eAccount("cn-work")),
			Upstream: up,
			AuthKeys: []AuthKey{{Key: "sk", Name: "e2e"}},
		})
		body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer sk")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body)
		}
		if len(urls) != 1 || !strings.Contains(urls[0], "www.workbuddy.cn") {
			t.Errorf("profile routing urls=%v want www.workbuddy.cn", urls)
		}
	})

	t.Run("image_policy_413", func(t *testing.T) {
		var urls []string
		h := NewHandler(Config{
			Pool:        testPoolWith(e2eAccount("cn-work")),
			Upstream:    e2eUpstream(t, &urls),
			AuthKeys:    []AuthKey{{Key: "sk", Name: "e2e"}},
			MaxImages:   1,
			ImagePolicy: "error",
		})
		body := `{"model":"glm-5.2","messages":[{"role":"user","content":[` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,BB"}}]}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer sk")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body)
		}
		assertJSONErrorCode(t, rec.Body.String(), "too_many_images")
		if len(urls) != 0 {
			t.Errorf("413 must not reach upstream, urls=%v", urls)
		}
	})

	t.Run("capability_preflight_400", func(t *testing.T) {
		resetModelsCache()
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = []upstream.ModelInfo{{
			ID: "glm-5.2", SupportsImages: false, SupportsImagesSet: true,
		}}
		dynamicModelsCache.fetched = time.Now()
		dynamicModelsCache.Unlock()
		t.Cleanup(resetModelsCache)

		var urls []string
		h := NewHandler(Config{
			Pool:            testPoolWith(e2eAccount("cn-work")),
			Upstream:        e2eUpstream(t, &urls),
			AuthKeys:        []AuthKey{{Key: "sk", Name: "e2e"}},
			CapabilityGuard: true,
		})
		body := `{"model":"glm-5.2","messages":[{"role":"user","content":[` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer sk")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
		}
		assertJSONErrorCode(t, rec.Body.String(), "model_capability_mismatch")
		if len(urls) != 0 {
			t.Errorf("capability 400 must not reach upstream, urls=%v", urls)
		}
	})

	t.Run("search_and_mcp", func(t *testing.T) {
		var urls []string
		h := NewHandler(Config{
			Pool:          testPoolWith(e2eAccount("cn-work")),
			Upstream:      e2eUpstream(t, &urls),
			AuthKeys:      []AuthKey{{Key: "sk", Name: "e2e"}},
			SearchEnabled: true,
			MCPEnabled:    true,
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/search", bytes.NewReader([]byte(`{"query":"x"}`)))
		req.Header.Set("Authorization", "Bearer sk")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "search_result") {
			t.Fatalf("search code=%d body=%s", rec.Code, rec.Body)
		}
		req = httptest.NewRequest(http.MethodPost, "/v1/mcp",
			bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
		req.Header.Set("Authorization", "Bearer sk")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "web_search") {
			t.Fatalf("mcp code=%d body=%s", rec.Code, rec.Body)
		}
	})
}
