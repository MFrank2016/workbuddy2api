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

// stampedPersonalJWT 拼一个 token_source=enterprise_switch 的 JWT（个人号、
// enterpriseId 为空）。该声明只记本次授权流程，不代表账号形态；2026-10-14
// 实测证伪「带此章的号行为事件被静默丢弃」，详见 auth.IsEnterprise 说明。
func stampedPersonalJWT() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1","token_source":"enterprise_switch","enterprise_id":""}`))
	return "hdr." + payload + ".sig"
}

// TestAutoTaskAllEnterpriseGate 一键完成全部任务只对**真企业版**（enterpriseId
// 非空）返回 422，且不产生任何上游调用（成长端点上游 403，跑一遍只是白烧真实
// 对话配额）。带 token_source=enterprise_switch 声明但 enterpriseId 为空的个人号、
// 以及普通个人号，都必须照常进入流水线（200 且访问上游）。
//
// 回归钉子：曾有版本把该 JWT 声明当「企业上下文」，据此拒绝个人号并提示
// 「请移除账号后以个人身份重新登录」——实测证伪，见 auth.IsEnterprise 说明。
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

	t.Run("real enterprise refused with zero upstream calls", func(t *testing.T) {
		var reqs int32
		a := &auth.Auth{UID: "ent", EnterpriseID: "ent-1", AccessToken: "at"}
		rec := callAutoAll(newPanel(t, a, &reqs), "ent")

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422；body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解响应失败: %v；body=%s", err, rec.Body.String())
		}
		want := "该账号为企业版，无个人成长体系（成长端点上游 403）；请改用个人账号"
		if body["error"] != want {
			t.Errorf("error = %q, want %q", body["error"], want)
		}
		if got := atomic.LoadInt32(&reqs); got != 0 {
			t.Errorf("上游请求数 = %d, want 0（门控须在任何上游调用之前）", got)
		}
	})

	t.Run("stamped personal session not gated", func(t *testing.T) {
		var reqs int32
		a := &auth.Auth{UID: "u1", AccessToken: stampedPersonalJWT()}
		rec := callAutoAll(newPanel(t, a, &reqs), "u1")

		if rec.Code != http.StatusOK {
			t.Fatalf("带 enterprise_switch 声明的个人号 status = %d, want 200；body=%s", rec.Code, rec.Body.String())
		}
		if got := atomic.LoadInt32(&reqs); got == 0 {
			t.Error("带声明的个人号应进入正常流水线并访问上游，实际请求数 = 0")
		}
	})

	t.Run("normal personal session unaffected", func(t *testing.T) {
		var reqs int32
		a := &auth.Auth{UID: "u1", AccessToken: "opaque-token"}
		rec := callAutoAll(newPanel(t, a, &reqs), "u1")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200；body=%s", rec.Code, rec.Body.String())
		}
		if got := atomic.LoadInt32(&reqs); got == 0 {
			t.Error("普通个人号应进入正常流水线并访问上游，实际请求数 = 0")
		}
	})
}
