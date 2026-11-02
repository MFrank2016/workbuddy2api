package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestEmbeddingsDisabled 关闭时端点仍在但回 403（不是 404）。
func TestEmbeddingsDisabled(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"bge-m3","input":"hi"}`)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "embeddings_disabled") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestEmbeddingsFillsOpenAIShape 上游缺 object/index，网关补齐；条数与输入一致。
func TestEmbeddingsFillsOpenAIShape(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":[{"embedding":[0.1]},{"embedding":[0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, EmbeddingsEnabled: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"bge-m3","input":["a","b"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("bad shape: %s", rec.Body)
	}
	for i, d := range out.Data {
		if d.Object != "embedding" || d.Index != i || len(d.Embedding) != 1 {
			t.Errorf("data[%d]=%+v", i, d)
		}
	}
}

// TestEmbeddingsModelNotFound 上游 11351 → OpenAI 404 model_not_found。
func TestEmbeddingsModelNotFound(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"code":11351,"msg":"embedding model [x] not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, EmbeddingsEnabled: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"x","input":"hi"}`)))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}
