// agentcfg 本机 AI 客户端一键配置器（吸收自 hub 的 wb_agents.py）：
// 把 Claude Code / Codex CLI / OpenCode / Crush / ZCode 的配置指向本网关。
// 写入前把原件存进状态目录（默认 ~/.wb-agent-config），-restore 可字节级还原。
//
// 用法：
//
//	go run ./cmd/agentcfg -client codex -base-url http://127.0.0.1:7863 -api-key KEY -model glm-5.2
//	go run ./cmd/agentcfg -client codex -restore
//
// DSH 的配置是多层 YAML patch，暂不支持（请按官方文档手工配置）。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	providerID   = "wb-proxy"
	providerName = "WorkBuddy2API"
)

type options struct {
	client   string
	baseURL  string
	apiKey   string
	model    string
	models   []string
	dryRun   bool
	restore  bool
	home     string
	stateDir string
}

func main() {
	var o options
	var models string
	flag.StringVar(&o.client, "client", "", "client id: claude-code|codex|opencode|crush|zcode")
	flag.StringVar(&o.baseURL, "base-url", "http://127.0.0.1:7863", "gateway base url")
	flag.StringVar(&o.apiKey, "api-key", "", "gateway api key")
	flag.StringVar(&o.model, "model", "", "default model id")
	flag.StringVar(&models, "models", "", "comma-separated model ids (default: -model)")
	flag.BoolVar(&o.dryRun, "dry-run", false, "print planned changes without writing")
	flag.BoolVar(&o.restore, "restore", false, "restore original config")
	flag.StringVar(&o.home, "home", "", "override home directory (testing)")
	flag.StringVar(&o.stateDir, "state-dir", "", "state/backup dir (default ~/.wb-agent-config)")
	flag.Parse()

	if o.client == "" {
		fail("missing -client")
	}
	if o.home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fail("resolve home: " + err.Error())
		}
		o.home = h
	}
	if o.stateDir == "" {
		o.stateDir = filepath.Join(o.home, ".wb-agent-config")
	}
	o.models = splitModels(models, o.model)
	if err := run(o); err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "agentcfg: "+msg)
	os.Exit(1)
}

func splitModels(raw, def string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, p := range strings.Split(raw, ",") {
		add(p)
	}
	if len(out) == 0 && def != "" {
		add(def)
	}
	return out
}

type clientSpec struct {
	files func(home string) map[string]string
	apply func(files map[string][]byte, o options) (map[string][]byte, error)
}

func clientFor(id string) (clientSpec, error) {
	switch id {
	case "claude-code":
		return claudeSpec(), nil
	case "codex":
		return codexSpec(), nil
	case "opencode":
		return opencodeSpec(), nil
	case "crush":
		return crushSpec(), nil
	case "zcode":
		return zcodeSpec(), nil
	case "dsh":
		return clientSpec{}, errors.New("dsh 暂不支持：其配置是多层 YAML patch，请按官方文档手工配置")
	default:
		return clientSpec{}, fmt.Errorf("unknown client %q（支持 claude-code/codex/opencode/crush/zcode）", id)
	}
}

func run(o options) error {
	spec, err := clientFor(o.client)
	if err != nil {
		return err
	}
	files := spec.files(o.home)
	if o.restore {
		return restore(o, files)
	}
	cur := map[string][]byte{}
	for name, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("read %s: %w", path, err)
			}
			b = nil
		}
		cur[name] = b
	}
	next, err := spec.apply(cur, o)
	if err != nil {
		return err
	}
	for name, path := range files {
		b, ok := next[name]
		if !ok {
			continue
		}
		if o.dryRun {
			fmt.Printf("--- %s ---\n%s\n", path, b)
			continue
		}
		if err := ensureOriginal(o, name, path); err != nil {
			return err
		}
		if err := writeFileAtomic(path, b); err != nil {
			return err
		}
		fmt.Printf("updated %s\n", path)
	}
	return nil
}

// ensureOriginal 首次写入前把原件存进状态目录（缺失文件写 .absent 标记）。
func ensureOriginal(o options, name, path string) error {
	dir := filepath.Join(o.stateDir, "orig", o.client)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	orig := filepath.Join(dir, name)
	if _, err := os.Stat(orig); err == nil {
		return nil
	}
	if _, err := os.Stat(orig + ".absent"); err == nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.WriteFile(orig+".absent", []byte{}, 0o600)
		}
		return err
	}
	return os.WriteFile(orig, b, 0o600)
}

func restore(o options, files map[string]string) error {
	dir := filepath.Join(o.stateDir, "orig", o.client)
	for name, path := range files {
		orig := filepath.Join(dir, name)
		if b, err := os.ReadFile(orig); err == nil {
			if o.dryRun {
				fmt.Printf("would restore %s\n", path)
				continue
			}
			if err := writeFileAtomic(path, b); err != nil {
				return err
			}
			fmt.Printf("restored %s\n", path)
			continue
		}
		if _, err := os.Stat(orig + ".absent"); err == nil {
			if o.dryRun {
				fmt.Printf("would remove %s\n", path)
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Printf("removed %s\n", path)
			continue
		}
		fmt.Printf("skip %s (no original recorded)\n", path)
	}
	return nil
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// urls 归一化网关地址：root（不带 /v1）与 v1（带 /v1）。
func urls(raw string) (root, v1 string) {
	root = strings.TrimRight(strings.TrimSpace(raw), "/")
	root = strings.TrimSuffix(root, "/v1")
	return root, root + "/v1"
}

func loadJSONObject(b []byte, what string) (map[string]any, error) {
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func dumpJSON(m map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func deepMerge(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

func subMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	nm := map[string]any{}
	m[key] = nm
	return nm
}

func subList(m map[string]any, key string) []any {
	if v, ok := m[key].([]any); ok {
		return v
	}
	nl := []any{}
	m[key] = nl
	return nl
}

// ---------- Claude Code ----------

func claudeSpec() clientSpec {
	return clientSpec{
		files: func(home string) map[string]string {
			dir := os.Getenv("CLAUDE_CONFIG_DIR")
			if dir == "" {
				dir = filepath.Join(home, ".claude")
			}
			return map[string]string{"settings": filepath.Join(dir, "settings.json")}
		},
		apply: func(files map[string][]byte, o options) (map[string][]byte, error) {
			m, err := loadJSONObject(files["settings"], "claude settings")
			if err != nil {
				return nil, err
			}
			root, _ := urls(o.baseURL)
			env := subMap(m, "env")
			env["ANTHROPIC_BASE_URL"] = root
			env["ANTHROPIC_AUTH_TOKEN"] = o.apiKey
			if o.model != "" {
				for _, k := range []string{
					"ANTHROPIC_MODEL",
					"ANTHROPIC_DEFAULT_SONNET_MODEL",
					"ANTHROPIC_DEFAULT_OPUS_MODEL",
					"ANTHROPIC_DEFAULT_HAIKU_MODEL",
				} {
					env[k] = o.model
				}
			}
			b, err := dumpJSON(m)
			if err != nil {
				return nil, err
			}
			return map[string][]byte{"settings": b}, nil
		},
	}
}

// ---------- Codex CLI ----------

func codexSpec() clientSpec {
	return clientSpec{
		files: func(home string) map[string]string {
			dir := os.Getenv("CODEX_HOME")
			if dir == "" {
				dir = filepath.Join(home, ".codex")
			}
			return map[string]string{
				"config": filepath.Join(dir, "config.toml"),
				"auth":   filepath.Join(dir, "auth.json"),
			}
		},
		apply: func(files map[string][]byte, o options) (map[string][]byte, error) {
			text := string(files["config"])
			text = tomlUpsertTopKey(text, "model_provider", providerID)
			if o.model != "" {
				text = tomlUpsertTopKey(text, "model", o.model)
			}
			_, v1 := urls(o.baseURL)
			text = tomlUpsertTable(text, "model_providers."+providerID, [][2]string{
				{"name", providerName},
				{"base_url", v1},
				{"wire_api", "responses"},
				{"experimental_bearer_token", o.apiKey},
			})
			auth, err := loadJSONObject(files["auth"], "codex auth")
			if err != nil {
				return nil, err
			}
			auth["OPENAI_API_KEY"] = o.apiKey
			ab, err := dumpJSON(auth)
			if err != nil {
				return nil, err
			}
			return map[string][]byte{"config": []byte(text), "auth": ab}, nil
		},
	}
}

func tomlString(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "\"", "\\\"")
	return "\"" + v + "\""
}

// tomlUpsertTopKey 在首个 [table] 之前插入/更新顶层 key（文本级，保留其余内容）。
func tomlUpsertTopKey(text, key, value string) string {
	lines := strings.Split(text, "\n")
	firstTable := len(lines)
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "[") {
			firstTable = i
			break
		}
	}
	top := lines[:firstTable]
	rest := lines[firstTable:]
	var out []string
	for _, ln := range top {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, key+" ") || strings.HasPrefix(trimmed, key+"=") || strings.HasPrefix(trimmed, key+" =") {
			continue
		}
		out = append(out, ln)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	out = append(out, key+" = "+tomlString(value), "")
	lines = append(out, rest...)
	return strings.Join(lines, "\n")
}

// tomlUpsertTable 替换（或追加）一个 [table] 块（块内键整体重写，env_key 等旧键随之清除）。
func tomlUpsertTable(text, table string, kv [][2]string) string {
	lines := strings.Split(text, "\n")
	header := "[" + table + "]"
	var out []string
	skipping := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "[") {
			if trimmed == header {
				skipping = true
				continue
			}
			skipping = false
			out = append(out, ln)
			continue
		}
		if skipping {
			continue
		}
		out = append(out, ln)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	out = append(out, "", header)
	for _, p := range kv {
		out = append(out, p[0]+" = "+tomlString(p[1]))
	}
	out = append(out, "")
	return strings.Join(out, "\n")
}

// ---------- OpenCode ----------

func opencodeSpec() clientSpec {
	return clientSpec{
		files: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".config", "opencode", "opencode.json")}
		},
		apply: func(files map[string][]byte, o options) (map[string][]byte, error) {
			m, err := loadJSONObject(files["config"], "opencode config")
			if err != nil {
				return nil, err
			}
			if _, ok := m["$schema"]; !ok {
				m["$schema"] = "https://opencode.ai/config.json"
			}
			_, v1 := urls(o.baseURL)
			provider := map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": providerName,
				"options": map[string]any{
					"baseURL": v1,
					"apiKey":  o.apiKey,
				},
			}
			if len(o.models) > 0 {
				models := map[string]any{}
				for _, id := range o.models {
					models[id] = map[string]any{}
				}
				provider["models"] = models
			}
			deepMerge(subMap(m, "provider"), map[string]any{providerID: provider})
			if o.model != "" {
				m["model"] = providerID + "/" + o.model
			}
			b, err := dumpJSON(m)
			if err != nil {
				return nil, err
			}
			return map[string][]byte{"config": b}, nil
		},
	}
}

// ---------- Crush ----------

func crushSpec() clientSpec {
	return clientSpec{
		files: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".config", "crush", "crush.json")}
		},
		apply: func(files map[string][]byte, o options) (map[string][]byte, error) {
			m, err := loadJSONObject(files["config"], "crush config")
			if err != nil {
				return nil, err
			}
			_, v1 := urls(o.baseURL)
			provider := map[string]any{
				"name":     providerName,
				"type":     "openai",
				"base_url": v1,
				"api_key":  o.apiKey,
			}
			if len(o.models) > 0 {
				models := make([]any, 0, len(o.models))
				for _, id := range o.models {
					models = append(models, map[string]any{"id": id, "name": id})
				}
				provider["models"] = models
			}
			deepMerge(subMap(m, "providers"), map[string]any{providerID: provider})
			b, err := dumpJSON(m)
			if err != nil {
				return nil, err
			}
			return map[string][]byte{"config": b}, nil
		},
	}
}

// ---------- ZCode ----------

func zcodeSpec() clientSpec {
	return clientSpec{
		files: func(home string) map[string]string {
			return map[string]string{"config": filepath.Join(home, ".zcode", "v2", "provider_config.json")}
		},
		apply: func(files map[string][]byte, o options) (map[string][]byte, error) {
			m, err := loadJSONObject(files["config"], "zcode provider config")
			if err != nil {
				return nil, err
			}
			if len(m) == 0 {
				m = map[string]any{
					"schemaVersion": 1,
					"config": map[string]any{
						"providerOrder":       []any{},
						"providerConfigRules": map[string]any{"providerRules": []any{}},
					},
				}
			}
			if _, ok := m["schemaVersion"]; !ok {
				m["schemaVersion"] = 1
			}
			cfg := subMap(m, "config")
			holder := subMap(cfg, "providerConfigRules")
			rules := subList(holder, "providerRules")
			_, v1 := urls(o.baseURL)
			rule := map[string]any{
				"providerId":   providerID,
				"providerName": providerName,
				"config": map[string]any{
					"group":  "standard-personal",
					"access": map[string]any{"type": "api-key", "apiKey": o.apiKey},
					"api":    map[string]any{"type": "openai-chat-completions", "baseUrl": v1},
				},
			}
			if len(o.models) > 0 {
				ids := make([]any, 0, len(o.models))
				for _, id := range o.models {
					ids = append(ids, id)
				}
				rc := rule["config"].(map[string]any)
				rc["personalModelIds"] = ids
				rc["modelOrder"] = ids
			}
			replaced := false
			for i, ex := range rules {
				if em, ok := ex.(map[string]any); ok && em["providerId"] == providerID {
					rules[i] = deepMerge(em, rule)
					replaced = true
					break
				}
			}
			if !replaced {
				rules = append(rules, rule)
			}
			holder["providerRules"] = rules
			order := subList(cfg, "providerOrder")
			found := false
			for _, v := range order {
				if v == providerID {
					found = true
					break
				}
			}
			if !found {
				order = append(order, providerID)
			}
			cfg["providerOrder"] = order
			b, err := dumpJSON(m)
			if err != nil {
				return nil, err
			}
			return map[string][]byte{"config": b}, nil
		},
	}
}
