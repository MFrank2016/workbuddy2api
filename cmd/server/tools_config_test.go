// tools_config_test.go Todo 11：图片/能力门禁/search/MCP 的配置与热生效接线。
//
// 这些字段全部**默认关闭**（0/passthrough/false/false/false），flag off 时行为与
// 迁移前逐字一致。配置层面的错误必须在**启动时**报出来：policy/mode 写错会让
// 上游起不来，而不是等到第一个请求才发现。
package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// TestToolsConfigDefaults 默认值：全关、policy=passthrough、file_mode 空（保持 0600）。
func TestToolsConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.MaxImages != 0 {
		t.Errorf("upstream.max_images=%d want 0", c.Upstream.MaxImages)
	}
	if c.Upstream.ImagePolicy != "passthrough" {
		t.Errorf("upstream.image_policy=%q want passthrough", c.Upstream.ImagePolicy)
	}
	if c.Upstream.CapabilityGuard {
		t.Error("upstream.capability_guard 默认必须为 false（不预检）")
	}
	if c.Search.Enabled {
		t.Error("search.enabled 默认必须为 false（/v1/search 404）")
	}
	if c.MCP.Enabled {
		t.Error("mcp.enabled 默认必须为 false（/v1/mcp 404）")
	}
	if c.Auth.FileMode != "" {
		t.Errorf("auth.file_mode=%q want empty（空 = 保持现状 0600）", c.Auth.FileMode)
	}
	if c.AuthFileModeSet {
		t.Error("auth.file_mode 未配置时不应设置进程级覆盖")
	}
}

// TestToolsConfigParsedFromFile JSON 键必须真的落到字段上（否则面板存了不生效）。
func TestToolsConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	raw := `{"upstream":{"max_images":8,"image_policy":"truncate","capability_guard":true},
	         "search":{"enabled":true},"mcp":{"enabled":true},"auth":{"file_mode":"0640"}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Upstream.MaxImages != 8 || c.Upstream.ImagePolicy != "truncate" || !c.Upstream.CapabilityGuard {
		t.Errorf("upstream=%+v", c.Upstream)
	}
	if !c.Search.Enabled || !c.MCP.Enabled {
		t.Errorf("search=%v mcp=%v want true/true", c.Search.Enabled, c.MCP.Enabled)
	}
	if c.Auth.FileMode != "0640" || !c.AuthFileModeSet || c.AuthFileMode != 0o640 {
		t.Errorf("auth.file_mode=%q parsed=%#o set=%v want 0640", c.Auth.FileMode, c.AuthFileMode, c.AuthFileModeSet)
	}
}

// TestToolsConfigEnvOverrides 六个 env 覆盖必须生效（容器部署只给 env）。
func TestToolsConfigEnvOverrides(t *testing.T) {
	t.Setenv("WB2A_MAX_IMAGES", "16")
	t.Setenv("WB2A_IMAGE_POLICY", "truncate")
	t.Setenv("WB2A_CAPABILITY_GUARD", "true")
	t.Setenv("WB2A_SEARCH_ENABLED", "true")
	t.Setenv("WB2A_MCP_ENABLED", "true")
	t.Setenv("WB2A_AUTH_FILE_MODE", "0644")

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Upstream.MaxImages != 16 {
		t.Errorf("max_images=%d want 16", c.Upstream.MaxImages)
	}
	if c.Upstream.ImagePolicy != "truncate" {
		t.Errorf("image_policy=%q want truncate", c.Upstream.ImagePolicy)
	}
	if !c.Upstream.CapabilityGuard || !c.Search.Enabled || !c.MCP.Enabled {
		t.Errorf("guard=%v search=%v mcp=%v want all true",
			c.Upstream.CapabilityGuard, c.Search.Enabled, c.MCP.Enabled)
	}
	if c.AuthFileMode != 0o644 {
		t.Errorf("auth file mode=%#o want 0644", c.AuthFileMode)
	}
}

// TestToolsConfigInvalidImagePolicy 非法 policy 必须启动失败（file 与 env 两条入口）。
func TestToolsConfigInvalidImagePolicy(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		if err := os.WriteFile(fp, []byte(`{"upstream":{"image_policy":"bogus"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(fp); err == nil {
			t.Fatal("want error for upstream.image_policy=bogus")
		} else if !strings.Contains(err.Error(), "image_policy") {
			t.Fatalf("错误信息应点名 image_policy: %v", err)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("WB2A_IMAGE_POLICY", "bogus")
		if _, err := Load(""); err == nil {
			t.Fatal("want error for WB2A_IMAGE_POLICY=bogus")
		}
	})
}

// TestToolsConfigNegativeMaxImages 负图片上限无意义，启动即拒。
func TestToolsConfigNegativeMaxImages(t *testing.T) {
	t.Setenv("WB2A_MAX_IMAGES", "-1")
	if _, err := Load(""); err == nil {
		t.Fatal("want error for max_images=-1（0 = 不限）")
	}
}

// TestToolsConfigInvalidAuthFileMode 只允许 0600/0640/0644。
func TestToolsConfigInvalidAuthFileMode(t *testing.T) {
	for _, bad := range []string{"0666", "0640x", "rw-r-----", "6000"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("WB2A_AUTH_FILE_MODE", bad)
			if _, err := Load(""); err == nil {
				t.Fatalf("want error for auth.file_mode=%q", bad)
			}
		})
	}
}

// TestAuthFileModeIsRestartOnly auth.file_mode 只在启动时接线：
//
//	· normalize()（面板保存也会走它）**不得**改变进程级覆盖 —— 否则面板一存配置，
//	  新建 auth 文件的权限当场变了，与「restart-only」的承诺相反；
//	· 启动期的 applyAuthFileMode 才真正调用 auth.SetFileMode。
func TestAuthFileModeIsRestartOnly(t *testing.T) {
	prev := auth.FileMode()
	t.Cleanup(func() { _ = auth.SetFileMode(prev) })

	if err := auth.SetFileMode(0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Auth.FileMode = "0644"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got := auth.FileMode(); got != 0o600 {
		t.Fatalf("normalize 改了进程级 file mode: %#o（必须是 restart-only）", got)
	}

	if err := applyAuthFileMode(c); err != nil {
		t.Fatalf("applyAuthFileMode: %v", err)
	}
	if got := auth.FileMode(); got != 0o644 {
		t.Fatalf("applyAuthFileMode 后 mode=%#o want 0644", got)
	}
}

// TestApplyAuthFileModeEmptyKeepsCurrent 空值 = 保持现状（不改进程级覆盖）。
func TestApplyAuthFileModeEmptyKeepsCurrent(t *testing.T) {
	prev := auth.FileMode()
	t.Cleanup(func() { _ = auth.SetFileMode(prev) })
	if err := auth.SetFileMode(0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if err := applyAuthFileMode(c); err != nil {
		t.Fatalf("applyAuthFileMode: %v", err)
	}
	if got := auth.FileMode(); got != 0o600 {
		t.Fatalf("空 auth.file_mode 改了 mode: %#o want 0600", got)
	}
}

// TestLiveSnapshotCarriesToolFlags liveSnapshot 是 main 与 saveConfig 的唯一映射点。
func TestLiveSnapshotCarriesToolFlags(t *testing.T) {
	c := Default()
	c.Upstream.MaxImages = 7
	c.Upstream.ImagePolicy = "error"
	c.Upstream.CapabilityGuard = true
	c.Search.Enabled = true
	c.MCP.Enabled = true
	c.APIKey = "k"
	c.Auth.FileMode = "0644"

	s := liveSnapshot(c)
	if s.MaxImages != 7 || s.ImagePolicy != "error" || !s.CapabilityGuard {
		t.Errorf("upstream 字段未进快照: %+v", s)
	}
	if !s.SearchEnabled || !s.MCPEnabled {
		t.Errorf("search/mcp 未进快照: %+v", s)
	}
	if s.APIKey != "k" {
		t.Errorf("既有字段丢失: %+v", s)
	}
	// auth.file_mode 是 restart-only：**不在** Snapshot 里（改值只能靠重启）。
	if _, ok := reflect.TypeOf(livecfg.Snapshot{}).FieldByName("AuthFileMode"); ok {
		t.Error("livecfg.Snapshot 不应包含 AuthFileMode（restart-only 字段）")
	}
	if _, ok := reflect.TypeOf(livecfg.Snapshot{}).FieldByName("FileMode"); ok {
		t.Error("livecfg.Snapshot 不应包含 FileMode（restart-only 字段）")
	}
}

// TestSaveConfigAppliesToolFlagsHot 面板保存后 5 个字段立即热生效。
func TestSaveConfigAppliesToolFlagsHot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(liveSnapshot(Default()))
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	raw := []byte(`{"upstream":{"max_images":9,"image_policy":"truncate","capability_guard":true},
	                "search":{"enabled":true},"mcp":{"enabled":true}}`)
	if _, err := saveConfig(raw, path, live, p, up, sch, false); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	s := live.Load()
	if s.MaxImages != 9 || s.ImagePolicy != "truncate" || !s.CapabilityGuard {
		t.Errorf("热生效快照 upstream 不对: %+v", s)
	}
	if !s.SearchEnabled || !s.MCPEnabled {
		t.Errorf("热生效快照 search/mcp 不对: %+v", s)
	}
}

// TestSaveConfigAuthFileModeNotHot 保存 auth.file_mode 不改变进程级覆盖（restart-only）。
func TestSaveConfigAuthFileModeNotHot(t *testing.T) {
	prev := auth.FileMode()
	t.Cleanup(func() { _ = auth.SetFileMode(prev) })
	if err := auth.SetFileMode(0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"k","auth":{"file_mode":"0600"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(liveSnapshot(Default()))
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	if _, err := saveConfig([]byte(`{"auth":{"file_mode":"0644"}}`), path, live, p, up, sch, false); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := auth.FileMode(); got != 0o600 {
		t.Fatalf("面板保存改了 auth 文件权限: %#o（必须重启才生效）", got)
	}
}

// TestConfigExampleHasToolFlags config.example.json 是容器默认配置，必须带新键。
func TestConfigExampleHasToolFlags(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json 应能被 Load 解析: %v", err)
	}
	if c.Upstream.ImagePolicy != "passthrough" {
		t.Errorf("example image_policy=%q want passthrough", c.Upstream.ImagePolicy)
	}
	if c.Upstream.MaxImages != 0 || c.Upstream.CapabilityGuard || c.Search.Enabled || c.MCP.Enabled {
		t.Errorf("example 默认值应为全关: %+v %+v %+v", c.Upstream, c.Search, c.MCP)
	}
	if c.Auth.FileMode != "" {
		t.Errorf("example auth.file_mode=%q want empty", c.Auth.FileMode)
	}
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"max_images"`, `"image_policy"`, `"capability_guard"`, `"search"`, `"mcp"`, `"file_mode"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("config.example.json 缺键 %s", key)
		}
	}
}
