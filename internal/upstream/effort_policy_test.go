package upstream

import (
	"encoding/json"
	"testing"
)

func TestResolveGatewayEffort(t *testing.T) {
	supported := []string{"low", "medium", "high"}
	cases := []struct{ want, expect string }{
		{"max", "high"},
		{"", "high"},
		{"high", "high"},
		{"medium", "medium"},
		{"low", "low"},
		{"off", "off"},
		{"none", "off"},
		{"disabled", "off"},
	}
	for _, tc := range cases {
		if got := ResolveGatewayEffort(tc.want, supported); got != tc.expect {
			t.Errorf("ResolveGatewayEffort(%q)=%q want %q", tc.want, got, tc.expect)
		}
	}
	// 不支持的具体档：取 ≤ 该档的最高支持档。
	if got := ResolveGatewayEffort("xhigh", supported); got != "high" {
		t.Errorf("xhigh → %q want high", got)
	}
	// 空支持列表：不改写。
	if got := ResolveGatewayEffort("high", nil); got != "" {
		t.Errorf("empty supported → %q want empty", got)
	}
}

func TestEffortPolicyNormalized(t *testing.T) {
	p := EffortPolicy{DefaultEffort: " MAX ", OverrideScope: "weird", ModelDefaults: map[string]string{
		" glm-5.2 ": "HIGH", "x": "inherit", "": "high",
	}}.Normalized()
	if p.DefaultEffort != "max" {
		t.Errorf("default_effort=%q want max", p.DefaultEffort)
	}
	if p.OverrideScope != OverrideScopeLevel {
		t.Errorf("override_scope=%q want level", p.OverrideScope)
	}
	if len(p.ModelDefaults) != 1 || p.ModelDefaults["glm-5.2"] != "high" {
		t.Errorf("model_defaults=%v want only glm-5.2→high", p.ModelDefaults)
	}
}

// TestApplyEffortPolicyClientPriority 客户端优先：客户端发了就保留，缺档才补默认。
func TestApplyEffortPolicyClientPriority(t *testing.T) {
	pc := EffortPolicy{Enabled: true, DefaultEffort: "max", ClientPriority: true}.Normalized()
	p := &pc
	obj := map[string]any{"reasoning_effort": "low"}
	applyEffortPolicy(obj, "m", []string{"low", "high"}, p, "cn")
	if obj["reasoning_effort"] != "low" {
		t.Errorf("client priority must keep low, got %v", obj["reasoning_effort"])
	}
	obj2 := map[string]any{}
	applyEffortPolicy(obj2, "m", []string{"low", "high"}, p, "cn")
	if obj2["reasoning_effort"] != "high" {
		t.Errorf("missing effort must be filled with high, got %v", obj2["reasoning_effort"])
	}
}

// TestApplyEffortPolicyGatewayPriority 网关优先：改写客户端档位；level 尊重关闭，
// all 连关闭一起覆盖。
func TestApplyEffortPolicyGatewayPriority(t *testing.T) {
	pc := EffortPolicy{Enabled: true, DefaultEffort: "max", ClientPriority: false, OverrideScope: OverrideScopeLevel}.Normalized()
	p := &pc
	obj := map[string]any{"reasoning_effort": "low"}
	applyEffortPolicy(obj, "m", []string{"low", "high"}, p, "cn")
	if obj["reasoning_effort"] != "high" {
		t.Errorf("gateway priority must rewrite low→high, got %v", obj["reasoning_effort"])
	}

	// level：客户端显式 off 保留。
	off := map[string]any{"reasoning_effort": "off"}
	applyEffortPolicy(off, "m", []string{"low", "high"}, p, "cn")
	if off["reasoning_effort"] != "off" {
		t.Errorf("level scope must respect client off, got %v", off["reasoning_effort"])
	}

	// all：客户端 off 被改写成开启 + 目标档。
	allc := EffortPolicy{Enabled: true, DefaultEffort: "max", OverrideScope: OverrideScopeAll}.Normalized()
	all := &allc
	obj3 := map[string]any{"reasoning_effort": "off", "thinking": map[string]any{"type": "disabled"}}
	applyEffortPolicy(obj3, "m", []string{"low", "high"}, all, "cn")
	if obj3["reasoning_effort"] != "high" {
		t.Errorf("all scope must rewrite off→high, got %v", obj3["reasoning_effort"])
	}
	if th, _ := obj3["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Errorf("all scope must enable thinking, got %v", obj3["thinking"])
	}
}

// TestPrepareBodyOptWithPolicyWired 策略真的进入出站 body（prepareBodyOptCore 接线）。
func TestPrepareBodyOptWithPolicyWired(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[],"reasoning_effort":"low"}`)
	pc := EffortPolicy{Enabled: true, DefaultEffort: "max", ClientPriority: false}.Normalized()
	p := &pc
	out := PrepareBodyOptWithPolicy(in, false, map[string][]string{"glm-5.2": {"low", "high"}}, nil, "cn", p)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "high" {
		t.Errorf("policy not applied through pipeline: %v", obj["reasoning_effort"])
	}
	// 策略关闭（nil）时保持旧行为：客户端 low 原样（injectThinking/normalize 不越权）。
	out2 := PrepareBodyOptWithPolicy(in, false, map[string][]string{"glm-5.2": {"low", "high"}}, nil, "cn", nil)
	var obj2 map[string]any
	if err := json.Unmarshal(out2, &obj2); err != nil {
		t.Fatal(err)
	}
	if obj2["reasoning_effort"] != "low" {
		t.Errorf("nil policy must keep client effort, got %v", obj2["reasoning_effort"])
	}
}
