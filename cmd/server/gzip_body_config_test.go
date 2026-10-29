// gzip_body_config_test.go upstream.gzip_body（issue #297）的配置契约。
//
// 该键**缺省关闭**：小请求不压缩，排障时也不会多一层编码；显式 true 才压缩。
// 与 upstream.* 其余字段一致，接线在启动期，故列入 restart-required 清单。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// TestGzipBodyConfigDefaultsOff 缺省必须是 false（可覆盖），不能跟随上游补丁的 true。
func TestGzipBodyConfigDefaultsOff(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.GzipBody {
		t.Fatal("upstream.gzip_body 默认必须为 false")
	}
}

// TestGzipBodyConfigParsedAndOverridable JSON 键落盘可读，env 可覆盖。
func TestGzipBodyConfigParsedAndOverridable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"upstream":{"gzip_body":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Upstream.GzipBody {
		t.Fatal("upstream.gzip_body=true 未落到字段")
	}

	t.Setenv("WB2A_GZIP_BODY", "false")
	c2, err := Load(fp)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if c2.Upstream.GzipBody {
		t.Fatal("WB2A_GZIP_BODY=false 应覆盖文件里的 true")
	}
}

// TestSaveConfigPersistsGzipBody 面板保存可落盘该键，并按 restart-only 提示重启
// （启动期接线，热更会读写竞态）。
func TestSaveConfigPersistsGzipBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(liveSnapshot(Default()))
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	restart, err := saveConfig([]byte(`{"upstream":{"gzip_body":true}}`), path, live, p, up, sch, true)
	if err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	found := false
	for _, f := range restart {
		if f == "upstream.gzip_body" {
			found = true
		}
	}
	if !found {
		t.Fatalf("upstream.gzip_body 应列入重启清单，got %v", restart)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"gzip_body": true`) {
		t.Fatalf("配置文件未落盘 gzip_body：%s", raw)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !c.Upstream.GzipBody {
		t.Fatal("重新读取后 gzip_body 应为 true")
	}
}
