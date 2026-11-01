package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestChatAllowedModelRejected models.allowed 白名单：白名单外模型点名调用直接
// 400 拒绝，不打上游、不占选号；白名单内模型照常放行。
func TestChatAllowedModelRejected(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsAllowed: []string{"glm-5.2"}})

	post := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		body := []byte(`{"model":"` + model + `","messages":[]}`)
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
		return rec
	}

	// 白名单外 → 400 model_unavailable（裸名 / 前缀写法都拒绝）
	for _, model := range []string{"deepseek-v3", "cn:deepseek-v3", "global:deepseek-v3"} {
		rec := post(model)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("model %q: code=%d, want 400", model, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Errorf("model %q: body=%s, want model_unavailable", model, rec.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("allowlist-rejected models must not reach upstream: calls=%d", calls)
	}

	// 白名单内 → 放行并打到上游
	if rec := post("glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("glm-5.2: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("allowed model: upstream calls=%d, want 1", calls)
	}
}

// TestModelsAllowedFiltered models.allowed 白名单：/v1/models 列表只列白名单内
// 模型。用包级 dynamicModelsCache 预置目录，避免联网拉取。
func TestModelsAllowedFiltered(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "deepseek-v3"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"data":[]}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsAllowed: []string{"glm-5.2"}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	if !ids["cn:glm-5.2"] {
		t.Errorf("allowlisted model missing from /v1/models: %v", ids)
	}
	if ids["cn:deepseek-v3"] {
		t.Errorf("model outside allowlist leaked into /v1/models: %v", ids)
	}
}

// TestAllowedModelDisabledByDefault 空配置 = 白名单不启用（向后兼容）：
// 任意模型都放行。
func TestAllowedModelDisabledByDefault(t *testing.T) {
	h := NewHandler(Config{})
	for _, m := range []string{"glm-5.2", "cn:deepseek-v3", "global:whatever"} {
		realm, bare := "cn", m
		if strings.HasPrefix(m, "global:") {
			realm, bare = "global", strings.TrimPrefix(m, "global:")
		} else if strings.HasPrefix(m, "cn:") {
			bare = strings.TrimPrefix(m, "cn:")
		}
		if !h.allowedModel(realm, bare) {
			t.Errorf("empty allowlist must allow %q", m)
		}
	}
}
