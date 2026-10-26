package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// resetCatalogCache 清空进程级动态模型缓存，避免用例间串扰（同名 helper 已存在于 handler_test.go）。
func resetCatalogCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

func newCatalogHandler(base string) *Handler {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	c := upstream.New()
	c.ChatBaseCN = base
	return NewHandler(Config{Pool: p, Upstream: c})
}

// TestModelsCatalogWarn 验证 CN 模型目录不可用时每次负缓存窗口只打一条 WARN，
// 且「HTTP 200 + 空列表」也算失败（上游把空列表包装成 error 返回）。
func TestModelsCatalogWarn(t *testing.T) {
	cases := []struct {
		name    string
		base    func() string
		wantLog int
	}{
		{
			name:    "transport error",
			base:    func() string { return "http://127.0.0.1:1" },
			wantLog: 1,
		},
		{
			name: "200 with empty list",
			base: func() string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				t.Cleanup(srv.Close)
				return srv.URL
			},
			wantLog: 1,
		},
		{
			name: "200 with models",
			base: func() string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"m1"}]}}`))
				}))
				t.Cleanup(srv.Close)
				return srv.URL
			},
			wantLog: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCatalogCache()
			h := newCatalogHandler(tc.base())

			var buf bytes.Buffer
			old := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(old)

			h.fetchDynamicModels()
			h.fetchDynamicModels() // 负缓存窗口内：不得重复告警

			if got := strings.Count(buf.String(), "模型目录"); got != tc.wantLog {
				t.Fatalf("want %d WARN(s) mentioning 模型目录, got %d; log=%s", tc.wantLog, got, buf.String())
			}
		})
	}
}
