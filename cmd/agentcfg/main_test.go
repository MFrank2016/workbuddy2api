package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseOpts(home, state string) options {
	return options{
		baseURL:  "http://127.0.0.1:7863/v1",
		apiKey:   "k",
		model:    "glm-5.2",
		models:   []string{"glm-5.2"},
		home:     home,
		stateDir: state,
	}
}

func TestClaudeApplyRestore(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := []byte(`{"env":{"KEEP":"1"},"permissions":{}}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(home, state)
	o.client = "claude-code"
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	env := m["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:7863" {
		t.Fatalf("base=%v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "k" || env["KEEP"] != "1" {
		t.Fatalf("env=%v", env)
	}
	if env["ANTHROPIC_MODEL"] != "glm-5.2" {
		t.Fatalf("model env=%v", env["ANTHROPIC_MODEL"])
	}
	// 还原必须字节级一致。
	if err := run(options{client: "claude-code", restore: true, home: home, stateDir: state}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(orig) {
		t.Fatalf("restore mismatch:\n%s", got)
	}
}

func TestCodexApplyTwiceAndRestore(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.toml")
	orig := "model = \"gpt-5\"\n\n[model_providers.other]\nname = \"x\"\n"
	if err := os.WriteFile(cfg, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(home, state)
	o.client = "codex"
	for i := 0; i < 2; i++ {
		if err := run(o); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(cfg)
	text := string(b)
	if strings.Count(text, "[model_providers.wb-proxy]") != 1 {
		t.Fatalf("duplicate table:\n%s", text)
	}
	if !strings.Contains(text, `model_provider = "wb-proxy"`) ||
		!strings.Contains(text, `base_url = "http://127.0.0.1:7863/v1"`) ||
		!strings.Contains(text, `experimental_bearer_token = "k"`) {
		t.Fatalf("config missing fields:\n%s", text)
	}
	if !strings.Contains(text, "[model_providers.other]") {
		t.Fatalf("user table lost:\n%s", text)
	}
	auth := filepath.Join(dir, "auth.json")
	if ab, err := os.ReadFile(auth); err != nil || !strings.Contains(string(ab), "OPENAI_API_KEY") {
		t.Fatalf("auth.json=%s err=%v", ab, err)
	}
	// 还原：config 字节级一致，原本不存在的 auth.json 应被删除。
	if err := run(options{client: "codex", restore: true, home: home, stateDir: state}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	if string(got) != orig {
		t.Fatalf("restore mismatch:\n%s", got)
	}
	if _, err := os.Stat(auth); !os.IsNotExist(err) {
		t.Fatalf("auth.json should be removed on restore")
	}
}

func TestJSONClientsApply(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	o := baseOpts(home, state)
	for _, c := range []string{"opencode", "crush", "zcode"} {
		o.client = c
		if err := run(o); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}

	var oc map[string]any
	b, _ := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err := json.Unmarshal(b, &oc); err != nil {
		t.Fatal(err)
	}
	prov := oc["provider"].(map[string]any)["wb-proxy"].(map[string]any)
	opts := prov["options"].(map[string]any)
	if opts["baseURL"] != "http://127.0.0.1:7863/v1" || opts["apiKey"] != "k" {
		t.Fatalf("opencode provider=%v", prov)
	}
	if oc["model"] != "wb-proxy/glm-5.2" {
		t.Fatalf("opencode model=%v", oc["model"])
	}

	var cr map[string]any
	b, _ = os.ReadFile(filepath.Join(home, ".config", "crush", "crush.json"))
	if err := json.Unmarshal(b, &cr); err != nil {
		t.Fatal(err)
	}
	cp := cr["providers"].(map[string]any)["wb-proxy"].(map[string]any)
	if cp["type"] != "openai" || cp["base_url"] != "http://127.0.0.1:7863/v1" || cp["api_key"] != "k" {
		t.Fatalf("crush provider=%v", cp)
	}

	var zc map[string]any
	b, _ = os.ReadFile(filepath.Join(home, ".zcode", "v2", "provider_config.json"))
	if err := json.Unmarshal(b, &zc); err != nil {
		t.Fatal(err)
	}
	rules := zc["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("zcode rules=%v", rules)
	}
	r0 := rules[0].(map[string]any)
	if r0["providerId"] != "wb-proxy" {
		t.Fatalf("zcode rule=%v", r0)
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	o := baseOpts(home, state)
	o.client = "crush"
	o.dryRun = true
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "crush", "crush.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote %s", path)
	}
}
