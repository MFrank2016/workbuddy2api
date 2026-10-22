package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestProfilesDefaults 产品 profile 开关默认**全关**（契约 §5：迁移前行为逐字一致）。
//
// enabled=false 时 core 完全走旧 chatBase/globalOn；detect_only=true 只写字段。
// 两者都必须是 restart-only（见 TestProfilesAreRestartRequired），因为 URL 路由
// 的接线在启动期。
func TestProfilesDefaults(t *testing.T) {
	c := Default()
	if c.Profiles.Enabled {
		t.Error("profiles.enabled 默认必须为 false")
	}
	if !c.Profiles.DetectOnly {
		t.Error("profiles.detect_only 默认必须为 true（只检测不路由）")
	}
}

// TestProfilesNormalizeDefaults 缺键（老 config）时归一化后仍是关/仅检测。
func TestProfilesNormalizeDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Profiles.Enabled || !c.Profiles.DetectOnly {
		t.Errorf("缺键归一化后 enabled=%v detect_only=%v want false/true",
			c.Profiles.Enabled, c.Profiles.DetectOnly)
	}
}

// TestProfilesEnvOverrides 两个开关都要能被 env 覆盖（与 search/mcp 同风格）。
func TestProfilesEnvOverrides(t *testing.T) {
	t.Setenv("WB2A_PROFILES_ENABLED", "true")
	t.Setenv("WB2A_PROFILES_DETECT_ONLY", "false")
	c := Default()
	applyEnv(c)
	if !c.Profiles.Enabled {
		t.Error("WB2A_PROFILES_ENABLED=true 未生效")
	}
	if c.Profiles.DetectOnly {
		t.Error("WB2A_PROFILES_DETECT_ONLY=false 未生效")
	}
}

// TestProfilesAreRestartRequired 保存 profiles.* 必须返回 restart_required。
//
// 为什么不能热生效：profile 决定 base URL / Origin / UA 与排程门控，接线在
// 启动期（Todo 21）。热更它会让「面板说已保存、实际路由没变」，而这类不一致
// 从界面上完全看不出来。
func TestProfilesAreRestartRequired(t *testing.T) {
	for _, path := range []string{"profiles.enabled", "profiles.detect_only"} {
		found := false
		for _, p := range restartRequiredPaths {
			if p == path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("restartRequiredPaths 缺 %q", path)
		}
	}

	oldCfg := map[string]any{
		"profiles": map[string]any{"enabled": false, "detect_only": true},
	}
	newCfg := map[string]any{
		"profiles": map[string]any{"enabled": true, "detect_only": true},
	}
	got := restartRequiredFields(oldCfg, newCfg)
	if !reflect.DeepEqual(got, []string{"profiles.enabled"}) {
		t.Fatalf("got %v want [profiles.enabled]", got)
	}
	// 改 detect_only 同样要报
	newCfg2 := map[string]any{
		"profiles": map[string]any{"enabled": false, "detect_only": false},
	}
	if got := restartRequiredFields(oldCfg, newCfg2); !reflect.DeepEqual(got, []string{"profiles.detect_only"}) {
		t.Fatalf("got %v want [profiles.detect_only]", got)
	}
}

// TestAPIKeysProfilesField 密钥条目的 profiles 数组：JSON 往返 + 归一化 + 校验。
func TestAPIKeysProfilesField(t *testing.T) {
	raw := `{"api_key":"sk-main","api_keys":[` +
		`{"key":"sk-a","name":"A","profiles":["cn-cli","intl-cli"]},` +
		`{"key":"sk-b","name":"B"}]}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := cfg.normalizeAPIKeys(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !reflect.DeepEqual(cfg.APIKeys[0].Profiles, []string{"cn-cli", "intl-cli"}) {
		t.Errorf("profiles=%v want [cn-cli intl-cli]", cfg.APIKeys[0].Profiles)
	}
	if cfg.APIKeys[1].Profiles != nil {
		t.Errorf("省略 profiles 应收敛为 nil，实得 %#v", cfg.APIKeys[1].Profiles)
	}

	out, err := json.Marshal(cfg.APIKeys[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"profiles":["cn-cli","intl-cli"]`) {
		t.Errorf("JSON 未包含 profiles: %s", out)
	}
}

// TestNormalizeAPIKeysProfilesNormalization 归一：去空白、丢空串、按首次出现去重。
func TestNormalizeAPIKeysProfilesNormalization(t *testing.T) {
	cfg := &Config{APIKey: "sk-main", APIKeys: []APIKeyEntry{{
		Key:      "sk-a",
		Profiles: []string{" cn-cli ", "", "cn-cli", "intl-work", "  "},
	}}}
	if err := cfg.normalizeAPIKeys(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"cn-cli", "intl-work"}
	if !reflect.DeepEqual(cfg.APIKeys[0].Profiles, want) {
		t.Errorf("profiles=%#v want %#v", cfg.APIKeys[0].Profiles, want)
	}
}

// TestNormalizeAPIKeysRejectsBadProfile 非法 profile 值必须 fail-fast。
//
// 为什么不静默吃掉：密钥的 profiles 是**白名单**（空 = 不限）。把拼错的值
// 静默丢掉会让「本来想限制成 intl-cli」的密钥变成不限 —— 隔离静默失效，
// 正是本仓库反复出现的故障形态。
func TestNormalizeAPIKeysRejectsBadProfile(t *testing.T) {
	cases := []struct{ name, profile string }{
		{"拼错", "intl_cli"},
		{"未知", "cn-desktop"},
		{"大写", "CN-CLI"},
		{"通配以外的星号组合", "cn-*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{APIKey: "sk-main", APIKeys: []APIKeyEntry{{
				Key: "sk-a", Profiles: []string{c.profile},
			}}}
			err := cfg.normalizeAPIKeys()
			if err == nil {
				t.Fatalf("profile %q 应被拒绝", c.profile)
			}
			if !strings.Contains(err.Error(), "profiles") {
				t.Errorf("错误信息应点明 profiles: %v", err)
			}
		})
	}
	// "*" 是合法的「任意」哨兵（契约 §1 Selector）。
	cfg := &Config{APIKey: "sk-main", APIKeys: []APIKeyEntry{{
		Key: "sk-a", Profiles: []string{"*"},
	}}}
	if err := cfg.normalizeAPIKeys(); err != nil {
		t.Errorf(`profiles=["*"] 应合法: %v`, err)
	}
}

// TestBuildAuthKeysProfiles profiles 必须展开到 handler 的 AuthKey（含深拷贝来源）。
func TestBuildAuthKeysProfiles(t *testing.T) {
	cfg := &Config{
		APIKey: "sk-main",
		APIKeys: []APIKeyEntry{
			{Key: "sk-a", Name: "A", Profiles: []string{"cn-cli"}},
			{Key: "sk-b", Name: "B"},
		},
	}
	if err := cfg.normalizeAPIKeys(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	got := buildAuthKeys(cfg)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if len(got[0].Profiles) != 0 {
		t.Errorf("主密钥必须不限 profile，实得 %v", got[0].Profiles)
	}
	if !reflect.DeepEqual(got[1].Profiles, []string{"cn-cli"}) {
		t.Errorf("sk-a Profiles=%v want [cn-cli]", got[1].Profiles)
	}
	if got[2].Profiles != nil {
		t.Errorf("sk-b Profiles=%v want nil", got[2].Profiles)
	}
	// 展开是复制：改展开结果不得污染配置
	got[1].Profiles[0] = "intl-cli"
	if cfg.APIKeys[0].Profiles[0] != "cn-cli" {
		t.Error("buildAuthKeys 与配置共享了 Profiles 底层数组")
	}
}

// TestConfigExampleHasProfiles config.example.json（容器默认配置）必须带上
// 两个 profiles 键且取值与实现默认一致。
func TestConfigExampleHasProfiles(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json 应能被 Load 解析: %v", err)
	}
	if c.Profiles.Enabled {
		t.Error("config.example.json 的 profiles.enabled 应为 false")
	}
	if !c.Profiles.DetectOnly {
		t.Error("config.example.json 的 profiles.detect_only 应为 true")
	}
}
