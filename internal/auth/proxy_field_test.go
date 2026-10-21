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

// TestSaveAtomicPreservesProxySession 与 proxy 同一类坑：SaveAtomic 用固定字段集
// 整体重写文件，漏写 proxy_session 就会在每次 token 刷新时把它从磁盘删掉 ——
// SessionName() 随后退回昵称 slug，改过昵称的账号出口 IP 会悄悄变化。
func TestSaveAtomicPreservesProxySession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-sess.json")
	raw := []byte(`{
		"auth": {"accessToken":"t","refreshToken":"r","expiresAt":1,"domain":"d","realm":"cn"},
		"account": {"uid":"u","nickname":"n"},
		"proxy": "resin",
		"proxy_session": "sess-abc"
	}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.ProxySession != "sess-abc" {
		t.Fatalf("ProxySession = %q", a.ProxySession)
	}
	a.FilePath = path
	a.AccessToken = "t2"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	after, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := doc["proxy_session"].(string); got != "sess-abc" {
		t.Fatalf("SaveAtomic 丢了 proxy_session：%v", doc["proxy_session"])
	}
}

// TestSaveAtomicKeepsDiskProxyOverStaleMemory 锁定一个**实测发生过的竞态**：
//
// 面板在进程之外写 proxy，而本进程的 *Auth 最多滞后一个热加载周期（5s）。这期间
// 一次冷却/刷新写回若用内存里的旧空值，就会把面板刚绑好的线路抹掉（表现：
// 「绑定自己消失了」）。
func TestSaveAtomicKeepsDiskProxyOverStaleMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.json")
	// 磁盘上已有面板写入的 proxy / proxy_session
	onDisk := []byte(`{
		"auth": {"accessToken":"old","refreshToken":"r","expiresAt":1,"domain":"d","realm":"cn"},
		"account": {"uid":"u","nickname":"n"},
		"proxy": "resin",
		"proxy_session": "sess-x"
	}`)
	if err := os.WriteFile(path, onDisk, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 内存里是「面板写之前」的旧快照：没有 proxy / proxy_session
	stale := &Auth{AccessToken: "new", RefreshToken: "r", ExpiresAt: 2, Domain: "d",
		realm: "cn", UID: "u", Nickname: "n", FilePath: path}
	if err := stale.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	after, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := doc["proxy"].(string); got != "resin" {
		t.Fatalf("陈旧内存把磁盘上的 proxy 抹掉了：%v", doc["proxy"])
	}
	if got, _ := doc["proxy_session"].(string); got != "sess-x" {
		t.Fatalf("陈旧内存把磁盘上的 proxy_session 抹掉了：%v", doc["proxy_session"])
	}
	// token 仍然要更新
	authSec, _ := doc["auth"].(map[string]any)
	if got, _ := authSec["accessToken"].(string); got != "new" {
		t.Fatalf("accessToken 未更新：%q", got)
	}
}

// TestSaveAtomicDoesNotResurrectRemovedProxy 反向：面板**解绑**后，内存里残留的
// 旧线路名不能把绑定「复活」回来。
func TestSaveAtomicDoesNotResurrectRemovedProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unbound.json")
	onDisk := []byte(`{
		"auth": {"accessToken":"t","refreshToken":"r","expiresAt":1,"domain":"d","realm":"cn"},
		"account": {"uid":"u","nickname":"n"}
	}`)
	if err := os.WriteFile(path, onDisk, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	stale := &Auth{AccessToken: "t2", RefreshToken: "r", ExpiresAt: 1, Domain: "d",
		realm: "cn", UID: "u", Nickname: "n", FilePath: path, ProxyName: "resin"}
	if err := stale.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	after, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := doc["proxy"]; exists {
		t.Fatalf("已解绑的账号被内存里的旧线路名复活了：%v", doc["proxy"])
	}
}
