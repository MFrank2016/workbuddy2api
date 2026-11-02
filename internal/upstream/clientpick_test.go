package upstream

import (
	"reflect"
	"testing"
)

// TestParseV3ClientPickDefaultAgent 默认 agent（tags 含 default）的 models 是菜单
// 可见性口径：auto 剔除、paidId 并进 freeId、不在名单里的速度预设前置。
func TestParseV3ClientPickDefaultAgent(t *testing.T) {
	raw := []byte(`{"data":{"agents":[
		{"name":"other","models":["x"]},
		{"name":"cli","tags":["default"],"models":["auto","default-model","deepseek-v4.1-flash","deepseek-v4.1-flash-sg","fast-model"]}
	],"productFeaturesConfig":{"ModelRateLimitCap":{"lines":[{"freeId":"deepseek-v4.1-flash","paidId":"deepseek-v4.1-flash-sg"}]}}}}`)
	pick := parseV3ClientPick(raw)
	if pick == nil {
		t.Fatal("default agent must be picked")
	}
	got := pick.ordered()
	want := []string{"balanced-model", "deep-model", "default-model", "deepseek-v4.1-flash", "fast-model"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered=%v want %v", got, want)
	}
}

// TestParseV3ClientPickNoAgents 上游没下发 agents：返回 nil，调用方全量列出。
func TestParseV3ClientPickNoAgents(t *testing.T) {
	if pick := parseV3ClientPick([]byte(`{"data":{"models":[]}}`)); pick != nil {
		t.Fatalf("no agents must return nil, got %+v", pick)
	}
	if pick := parseV3ClientPick([]byte(`not json`)); pick != nil {
		t.Fatalf("bad json must return nil, got %+v", pick)
	}
}

// TestClientPickPresetOrder INTL 形态：fast/balanced 已在 agent 名单里，不前置，
// 结果是纯 agent 原序（首项 Auto 的 default-model）。
func TestClientPickPresetOrder(t *testing.T) {
	raw := []byte(`{"data":{"agents":[
		{"name":"cli","tags":["default"],"models":["default-model","fast-model","balanced-model","deepseek-v4.1-flash"]}
	]}}`)
	pick := parseV3ClientPick(raw)
	got := pick.ordered()
	want := []string{"deep-model", "default-model", "fast-model", "balanced-model", "deepseek-v4.1-flash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered=%v want %v", got, want)
	}
}
