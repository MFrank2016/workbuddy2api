package auth

import (
	"encoding/base64"
	"testing"
)

// mustStampedJWT 用给定 payload 拼一个最小 JWT（header.payload.signature）。
// 无需校验签名：门控只看 enterpriseId，这里保留 JWT 形态只是复现现网凭证。
func mustStampedJWT(t *testing.T, payload string) string {
	t.Helper()
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestEnterpriseContextOnlyDependsOnEnterpriseID 企业上下文一律以 enterpriseId
// 为准，绝不能按 JWT 的 token_source 声明判定。
//
// 回归钉子：token_source=enterprise_switch 只记录本次 OAuth 授权走了哪条流程，
// 不代表账号形态。2026-10-14 实测两个只带该章的个人号（enterprise_id 为空）
// chat_5 0/5→5/5、expert_5 0/5→5/5 等 /v2/report 行为事件全部正常计分领奖，
// WorkBuddy 客户端也显示「个人版」。曾按该声明把个人号误判为企业上下文，
// 导致成长任务被跳过、面板提示「请移除账号后以个人身份重新登录」。
func TestEnterpriseContextOnlyDependsOnEnterpriseID(t *testing.T) {
	cases := []struct {
		name         string
		enterpriseID string
		token        string
		want         bool
	}{
		{"个人号·带 enterprise_switch 声明", "", mustStampedJWT(t, `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":""}`), false},
		{"个人号·无声明", "", mustStampedJWT(t, `{"sub":"u1"}`), false},
		{"个人号·其他 token_source", "", mustStampedJWT(t, `{"sub":"u1","token_source":"oneid_login"}`), false},
		{"个人号·非 JWT 形态", "", "opaque-token", false},
		{"真企业版", "ent-1", mustStampedJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", EnterpriseID: c.enterpriseID, AccessToken: c.token}
			if got := a.IsEnterpriseSession(); got != c.want {
				t.Errorf("IsEnterpriseSession()=%v want %v（enterpriseId=%q token=%q）", got, c.want, c.enterpriseID, c.token)
			}
		})
	}
}

// TestStampedTokenDoesNotFlipIsEnterprise 带该声明的个人号不得翻转 IsEnterprise
// （额度查询仍走个人口径，不受 token 声明影响）。
func TestStampedTokenDoesNotFlipIsEnterprise(t *testing.T) {
	a := &Auth{UID: "u1", AccessToken: mustStampedJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`)}
	if a.IsEnterprise() {
		t.Error("仅 token_source=enterprise_switch 不应翻转 IsEnterprise（额度查询仍走个人口径）")
	}
}
