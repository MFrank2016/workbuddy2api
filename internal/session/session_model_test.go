package session

import (
	"testing"
	"time"
)

// TestResolveForModelKeepsBindingWhileCooling 绑定号只是**临时**不可用（软冷却/占满/
// 模型级限流）时，绑定不能被删掉——只让本轮返回 ok=false 由 handler 换号服务。
//
// 回归背景（2026-11-07 批量封禁事故）：绑定号一冷却就被 ResolveForModel 删绑定并
// 重新分配，于是同一条 900k 上下文的长对话在 12 个账号间接力，上游按「同一段私有
// 对话跨账号出现」判定异常行为，一次封掉 23/31 个账号。
//
// 断言两点：
//  1. 临时不可用 → ok=false（本轮换号，不空转）；
//  2. 绑定仍在（冷却结束后会话回归原号），且没有触发 DelBind。
func TestResolveForModelKeepsBindingWhileCooling(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string { return []string{"a2"} }
	r.Bind("c1", "a1")

	got, ok := r.ResolveForModel("c1", "hy4-preview")
	if ok {
		t.Fatalf("绑定号临时不可用时本轮不应返回它，got %s ok=%v", got, ok)
	}
	// 绑定必须还在 a1（未被删除/改绑）。
	r.mu.RLock()
	e, found := r.entries["c1"]
	r.mu.RUnlock()
	if !found || e.uid != "a1" {
		t.Fatalf("临时不可用不应摘除绑定：entries=%v found=%v", e, found)
	}
	if st.delBinds != 0 {
		t.Fatalf("临时不可用不应触发 DelBind，delBinds=%d", st.delBinds)
	}
}

// TestResolveForModelKeepsBindingOnModelLimit 绑定号在**该模型**上被限流时，本轮
// 解析失败（handler 换号服务），但绑定保留——切回原模型时会话回归原号。
//
// 语义变更（2026-11-07 封禁事故）：旧行为是「该模型不可用就重分配并改绑」，
// 那会让同一条长对话在模型/账号间反复搬迁。现在模型级限流属临时不可用。
func TestResolveForModelKeepsBindingOnModelLimit(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	// 模型维度可用性：hy4-preview 上只剩 a2（a1 被 6004 限额）；其他模型两个都在。
	r.cfg.AvailableForModel = func(model string) []string {
		if model == "hy4-preview" {
			return []string{"a2"}
		}
		return []string{"a1", "a2"}
	}

	// 模拟该会话此前在别的模型上绑定了 a1。
	r.Bind("c1", "a1")

	// 在 hy4-preview 上解析：a1 已限额 → 本轮不返回它。
	if got, ok := r.ResolveForModel("c1", "hy4-preview"); ok {
		t.Fatalf("绑定号在目标模型被限额时本轮不应返回它，got %s ok=%v", got, ok)
	}

	// 同一会话切回未限额的模型：必须仍回到 a1（绑定没被摘）。
	got, ok := r.ResolveForModel("c1", "glm-5.3")
	if !ok || got != "a1" {
		t.Errorf("ResolveForModel(glm-5.3)=%s ok=%v want a1/true（绑定应保留）", got, ok)
	}
}

// TestResolveForModelFallsBackToAvailable 未注入 AvailableForModel 时回落 Available
// （无模型维度，行为与引入前一致）。
func TestResolveForModelFallsBackToAvailable(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	got, ok := r.ResolveForModel("c1", "any-model")
	if !ok || got != "a1" {
		t.Errorf("无 AvailableForModel 时应回落 Available，got %s ok=%v", got, ok)
	}
}

// TestResolveForModelNoAvailableReturnsFalse 目标模型上无可用账号时返回 ok=false，
// 让 handler 回落普通轮换，而不是分配一个必然失败的号。
func TestResolveForModelNoAvailableReturnsFalse(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string {
		if model == "hy4-preview" {
			return nil
		}
		return []string{"a1"}
	}
	if _, ok := r.ResolveForModel("c1", "hy4-preview"); ok {
		t.Error("该模型无可用账号时应返回 ok=false")
	}
}

// TestResolveKeepsOldBehavior Resolve() 保留无模型语义（等价于空模型名），
// 保证既有调用方行为不变。
func TestResolveKeepsOldBehavior(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string { return []string{"a1"} }
	got, ok := r.Resolve("c1")
	if !ok || got != "a1" {
		t.Errorf("Resolve()=%s ok=%v want a1/true", got, ok)
	}
}
