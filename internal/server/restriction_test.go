package server

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// 11140 + 「账号存在异常行为，已被限制使用」= 上游明确封禁，需申诉；
// 与普通 request illegal 的软冷却分开处理。
func TestApplyErrorPolicyUpstreamRestrictionDisables(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	body := `{"error":{"data":{"code":11140,"msg":"request illegal","displayMsg":{"zh":"账号存在异常行为，已被限制使用"},"displayTips":{"zh":"如需申诉请提交反馈"}}}}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "glm-5.2", nil)

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("明确风控限制应禁用账号: %+v", st)
	}
	if st.Cooling {
		t.Fatalf("明确风控限制不应退化成冷却: %+v", st)
	}
	for _, want := range []string{"account banned", "appeal required"} {
		if !strings.Contains(st.DisabledReason, want) {
			t.Fatalf("disabled_reason=%q 缺少 %q", st.DisabledReason, want)
		}
	}
}
