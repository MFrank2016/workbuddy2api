package main

import (
	"reflect"
	"testing"
)

// TestRestartRequiredOnlyChangedFields 锁住一条容易被写成「固定清单」的判据：
//
// 保存配置后只应报告**本次确实变了**、且必须重启才生效的字段。此前这里返回
// 固定 17 项 —— 只改 schedule（热生效）也会被告知「这些字段需重启」，于是用户
// 被引导去做没必要的重启；久而久之这条提示就失去了可信度，改 listen 时反而
// 不会被当回事。
func TestRestartRequiredOnlyChangedFields(t *testing.T) {
	oldCfg := map[string]any{
		"listen": ":7863",
		"schedule": map[string]any{
			"checkin_hours": []any{float64(9)},
		},
		"server": map[string]any{"read_timeout": "300s"},
	}

	t.Run("只改热生效字段 → 无需重启", func(t *testing.T) {
		newCfg := map[string]any{
			"listen": ":7863",
			"schedule": map[string]any{
				"checkin_hours": []any{float64(9), float64(21)},
			},
			"server": map[string]any{"read_timeout": "300s"},
		}
		if got := restartRequiredFields(oldCfg, newCfg); len(got) != 0 {
			t.Fatalf("只改了 schedule，却报告需重启 %v（应为空）", got)
		}
	})

	t.Run("改了 listen → 只报 listen", func(t *testing.T) {
		newCfg := map[string]any{
			"listen": ":7864",
			"schedule": map[string]any{
				"checkin_hours": []any{float64(9)},
			},
			"server": map[string]any{"read_timeout": "300s"},
		}
		got := restartRequiredFields(oldCfg, newCfg)
		if !reflect.DeepEqual(got, []string{"listen"}) {
			t.Fatalf("got %v want [listen]", got)
		}
	})

	t.Run("改了嵌套字段 → 报该字段全路径", func(t *testing.T) {
		newCfg := map[string]any{
			"listen":   ":7863",
			"schedule": map[string]any{"checkin_hours": []any{float64(9)}},
			"server":   map[string]any{"read_timeout": "600s"},
		}
		got := restartRequiredFields(oldCfg, newCfg)
		if !reflect.DeepEqual(got, []string{"server.read_timeout"}) {
			t.Fatalf("got %v want [server.read_timeout]", got)
		}
	})

	t.Run("整段变化（upstash）→ 报段名", func(t *testing.T) {
		newCfg := map[string]any{
			"listen":   ":7863",
			"schedule": map[string]any{"checkin_hours": []any{float64(9)}},
			"server":   map[string]any{"read_timeout": "300s"},
			"upstash":  map[string]any{"url": "https://example"},
		}
		got := restartRequiredFields(oldCfg, newCfg)
		if !reflect.DeepEqual(got, []string{"upstash"}) {
			t.Fatalf("got %v want [upstash]", got)
		}
	})
}

// TestApplyIncomingDetectsChangeDespiteInPlaceMerge 锁住一个**真实的坑**：
//
// mergeConfigMaps 是原地深合并（直接改 cur）。如果调用方合并后再拿 cur 与
// merged 比较，两者是同一个 map，永远相等 —— restart_required 恒为空，
// 表现为「改了 listen 也不提示重启」。
//
// 这个用例走 applyIncoming（它会先深拷贝），确保改动能被识别出来。
func TestApplyIncomingDetectsChangeDespiteInPlaceMerge(t *testing.T) {
	cur := map[string]any{
		"listen":   ":7863",
		"server":   map[string]any{"read_timeout": "300s"},
		"schedule": map[string]any{"checkin_hours": []any{float64(9)}},
	}
	_, fields := applyIncoming(cur, map[string]any{
		"listen":   ":7864",
		"schedule": map[string]any{"checkin_hours": []any{float64(9), float64(21)}},
	})
	if !reflect.DeepEqual(fields, []string{"listen"}) {
		t.Fatalf("got %v want [listen]（原地合并让差异恒为空？）", fields)
	}
	// 纯热生效字段变化不应触发重启提示
	cur2 := map[string]any{
		"listen":   ":7863",
		"schedule": map[string]any{"checkin_hours": []any{float64(9)}},
	}
	if _, f2 := applyIncoming(cur2, map[string]any{
		"schedule": map[string]any{"checkin_hours": []any{float64(9), float64(21)}},
	}); len(f2) != 0 {
		t.Fatalf("只改 schedule 却要求重启：%v", f2)
	}
}
