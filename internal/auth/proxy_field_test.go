package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSaveAtomicPreservesProxy 锁住一个曾经真实发生的事故：
//
// SaveAtomic 用**固定字段集整体重写**账号文件，而 token 保活/签到是高频路径。
// 如果字段集里漏了 `proxy`，每次续期都会把面板绑好的出口线路静默解绑 ——
// 出口随之改变，而这类变化没人会发现（issue #210 的同一类问题）。
//
// 这个用例直接构造「文件里有 proxy」→ 解析 → SaveAtomic → 再读，断言仍在。
func TestSaveAtomicPreservesProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-11111111-2222-3333-4444-555555555555.json")

	original := map[string]any{
		"auth": map[string]any{
			"accessToken":  "token-abc",
			"refreshToken": "refresh-abc",
			"expiresAt":    int64(1700000000),
			"domain":       "www.codebuddy.cn",
			"realm":        "cn",
		},
		"account": map[string]any{
			"uid":      "11111111-2222-3333-4444-555555555555",
			"nickname": "tester",
		},
		"proxy": "resin",
	}
	raw, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.ProxyName != "resin" {
		t.Fatalf("ProxyName = %q, want %q", a.ProxyName, "resin")
	}
	a.FilePath = path

	// 模拟 token 刷新后的写回（高频路径）。
	a.AccessToken = "token-new"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := doc["proxy"].(string); got != "resin" {
		t.Fatalf("SaveAtomic 丢了 proxy：写回后 proxy=%v，期望 \"resin\"", doc["proxy"])
	}
	authSection, _ := doc["auth"].(map[string]any)
	if got, _ := authSection["accessToken"].(string); got != "token-new" {
		t.Fatalf("accessToken 未更新：%q", got)
	}
}

// TestParseFlatProxy 扁平形 auth 文件（手写/旧版）也要能读出 proxy。
func TestParseFlatProxy(t *testing.T) {
	raw := []byte(`{
		"accessToken": "t",
		"refreshToken": "r",
		"expiresAt": 1700000000,
		"domain": "www.codebuddy.cn",
		"uid": "u-1",
		"nickname": "n",
		"proxy": "  resin  "
	}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.ProxyName != "resin" {
		t.Fatalf("ProxyName = %q, want trimmed %q", a.ProxyName, "resin")
	}
}

// TestSaveAtomicOmitsEmptyProxy 没绑线路的文件不应被引入空 proxy 键
// （与 device_token / groups 同策略）。
func TestSaveAtomicOmitsEmptyProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wb.json")
	raw := []byte(`{
		"auth": {"accessToken":"t","refreshToken":"r","expiresAt":1,"domain":"d","realm":"cn"},
		"account": {"uid":"u","nickname":"n"}
	}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	after, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := doc["proxy"]; exists {
		t.Fatalf("未绑线路的文件不应写回空 proxy 键，实际: %v", doc["proxy"])
	}
}
