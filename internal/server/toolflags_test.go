// toolflags_test.go Todo 11：Live 快照为 nil 时的回落必须读 cfg，而不是硬编码。
//
// 为什么单测：Live 只在 cmd/server 接线，测试与裸用 Handler 的调用方拿到的
// fallback 若写死默认值，面板改了配置却在「无 Live」路径上永远显示旧值 ——
// 而这种不一致不会报错，只会让 flag 看起来没生效。
package server

import (
	"testing"

	"workbuddy2api/internal/livecfg"
)

// TestLoadLiveFallsBackToConfig Live==nil 时五个字段从 Config 取值。
func TestLoadLiveFallsBackToConfig(t *testing.T) {
	h := NewHandler(Config{
		APIKey:          "k",
		MaxImages:       12,
		ImagePolicy:     "truncate",
		CapabilityGuard: true,
		SearchEnabled:   true,
		MCPEnabled:      true,
	})
	s := h.loadLive()
	if s.MaxImages != 12 || s.ImagePolicy != "truncate" || !s.CapabilityGuard {
		t.Errorf("upstream 回落不对: %+v", s)
	}
	if !s.SearchEnabled || !s.MCPEnabled {
		t.Errorf("search/mcp 回落不对: %+v", s)
	}
}

// TestLoadLiveFallbackDefaults 零值 Config 的回落 = 全关 / passthrough。
func TestLoadLiveFallbackDefaults(t *testing.T) {
	h := NewHandler(Config{})
	s := h.loadLive()
	if s.MaxImages != 0 || s.ImagePolicy != "passthrough" || s.CapabilityGuard {
		t.Errorf("默认回落不对: %+v", s)
	}
	if s.SearchEnabled || s.MCPEnabled {
		t.Errorf("默认回落必须关闭: %+v", s)
	}
}

// TestLoadLivePrefersHolder Live 非空时以快照为准（回落只在 Live==nil 生效）。
func TestLoadLivePrefersHolder(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{
		MaxImages:       3,
		ImagePolicy:     "error",
		CapabilityGuard: false,
		SearchEnabled:   false,
		MCPEnabled:      true,
	})
	h := NewHandler(Config{
		Live:            live,
		MaxImages:       99,
		ImagePolicy:     "truncate",
		CapabilityGuard: true,
		SearchEnabled:   true,
		MCPEnabled:      false,
	})
	s := h.loadLive()
	if s.MaxImages != 3 || s.ImagePolicy != "error" || s.CapabilityGuard {
		t.Errorf("Live 快照应优先: %+v", s)
	}
	if s.SearchEnabled || !s.MCPEnabled {
		t.Errorf("Live 快照应优先: %+v", s)
	}
}
