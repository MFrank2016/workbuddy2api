package panel

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// enterpriseSessionJWT 拼一个 token_source=enterprise_switch 的最小 JWT。
// IsEnterpriseSession 不校验签名，payload 之外的两段可为任意占位。
func enterpriseSessionJWT() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"ent","token_source":"enterprise_switch"}`))
	return "hdr." + payload + ".sig"
}

// TestAutoTaskAllEnterpriseGate 一键完成全部任务必须与企业切换会话的
// 单任务入口同口径拒绝：返回同样的 422 文案，且不产生任何上游调用
// （企业会话的行为事件会被上游静默丢弃，跑一遍只是白烧真实对话配额）。
// 普通个人会话不受影响，仍进入正常流水线。
func TestAutoTaskAllEnterpriseGate(t *testing.T) {
	newPanel := func(t *testing.T, a *auth.Auth, reqs *int32) *Panel {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(reqs, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK", "data": map[string]any{"tasks": []any{}},
			})
		}))
		t.Cleanup(srv.Close)
		pl := pool.New("")
		pl.Add(a)
		return &Panel{cfg: Config{
			Pool: pl,
			Upstream: &upstream.Client{
				HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
			},
		}}
	}
	callAutoAll := func(p *Panel, uid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/"+uid+"/tasks/auto_all", nil)
		req.SetPathValue("uid", uid)
		rec := httptest.NewRecorder()
		p.accountTaskAutoAll(rec, req)
		return rec
	}

	t.Run("enterprise-switched session refused with zero upstream calls", func(t *testing.T) {
		var reqs int32
		a := &auth.Auth{UID: "ent", AccessToken: enterpriseSessionJWT()}
		rec := callAutoAll(newPanel(t, a, &reqs), "ent")

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422；body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解响应失败: %v；body=%s", err, rec.Body.String())
		}
		want := "该账号为企业版/企业切换会话（token_source=enterprise_switch），无个人成长体系，成长任务行为事件会被上游忽略；请移除账号后以个人身份重新登录"
		if body["error"] != want {
			t.Errorf("error = %q, want %q（须与单任务入口同文案）", body["error"], want)
		}
		if got := atomic.LoadInt32(&reqs); got != 0 {
			t.Errorf("上游请求数 = %d, want 0（门控须在任何上游调用之前）", got)
		}
	})

	t.Run("normal session unaffected", func(t *testing.T) {
		var reqs int32
		a := &auth.Auth{UID: "u1", AccessToken: "opaque-token"}
		rec := callAutoAll(newPanel(t, a, &reqs), "u1")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200；body=%s", rec.Code, rec.Body.String())
		}
		if got := atomic.LoadInt32(&reqs); got == 0 {
			t.Error("普通会话应进入正常流水线并访问上游，实际请求数 = 0")
		}
	})
}
