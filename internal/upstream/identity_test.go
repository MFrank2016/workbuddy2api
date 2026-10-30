package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func identityOpts(repl map[string]string) IdentityOptions {
	return IdentityOptions{Enabled: true, Target: "WorkBuddy", Replacements: repl}
}

func TestIdentityOnlySystemAssistant(t *testing.T) {
	body := []byte(`{"messages":[
	  {"role":"system","content":"You are Claude Code, Anthropic's CLI."},
	  {"role":"user","content":"Claude Code is great"},
	  {"role":"assistant","content":[{"type":"text","text":"I am Codex"},{"type":"reasoning","text":"Codex reasoning"},{"type":"image_url","image_url":{"url":"Codex.png"}}]},
	  {"role":"developer","content":"Codex developer note"},
	  {"role":"tool","content":"Codex output"}
	]}`)
	out := ApplyIdentity(body, identityOpts(map[string]string{"Claude Code": "WorkBuddy", "Codex": "WorkBuddy"}))
	s := string(out)
	for _, want := range []string{"You are WorkBuddy", "Claude Code is great", "I am WorkBuddy", "WorkBuddy developer note"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	for _, want := range []string{"Codex reasoning", "Codex.png", "Codex output"} {
		if !strings.Contains(s, want) {
			t.Fatalf("must be untouched %q in %s", want, s)
		}
	}
}

func TestIdentityPreservesNonMessageFields(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","temperature":0.7,"max_tokens":123,"stream":true,` +
		`"metadata":{"trace":"abc"},"tools":[{"type":"function","function":{"name":"Codex"}}],` +
		`"messages":[{"role":"system","content":"You are Codex","extra":42}]}`)
	out := ApplyIdentity(body, identityOpts(map[string]string{"Codex": "WorkBuddy"}))
	if !strings.Contains(string(out), `"You are WorkBuddy"`) {
		t.Fatalf("identity not applied: %s", out)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if root["model"] != "glm-5.2" || root["temperature"] != 0.7 || root["max_tokens"] != float64(123) || root["stream"] != true {
		t.Fatalf("non-message fields changed: %#v", root)
	}
	meta, _ := root["metadata"].(map[string]any)
	if meta["trace"] != "abc" {
		t.Fatalf("metadata changed: %#v", root["metadata"])
	}
	tools, _ := root["tools"].([]any)
	if len(tools) != 1 || !strings.Contains(string(out), `"name":"Codex"`) {
		t.Fatalf("tools changed: %s", out)
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages changed: %#v", root["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["extra"] != float64(42) {
		t.Fatalf("message extra field changed: %#v", msg)
	}
}

func TestIdentitySkipsCodeBlocks(t *testing.T) {
	in := "```\nYou are Claude Code\n```\nUse `Claude Code` here.\nYou are Claude Code."
	out := ReplaceIdentityText(in, identityOpts(map[string]string{"Claude Code": "WorkBuddy"}))
	if !strings.Contains(out, "```\nYou are Claude Code\n```") {
		t.Fatalf("fenced code changed: %s", out)
	}
	if !strings.Contains(out, "`Claude Code`") {
		t.Fatalf("inline code changed: %s", out)
	}
	if !strings.Contains(out, "You are WorkBuddy.") {
		t.Fatalf("prose not replaced: %s", out)
	}
}

func TestIdentitySkipsIndentedCode(t *testing.T) {
	in := "    You are Codex\n\tYou are Codex\nYou are Codex"
	out := ReplaceIdentityText(in, identityOpts(map[string]string{"Codex": "WorkBuddy"}))
	want := "    You are Codex\n\tYou are Codex\nYou are WorkBuddy"
	if out != want {
		t.Fatalf("indented code not preserved:\n got %q\nwant %q", out, want)
	}
}

func TestIdentityIdempotent(t *testing.T) {
	opt := identityOpts(map[string]string{"Codex": "WorkBuddy"})
	once := ReplaceIdentityText("You are Codex. Codex.", opt)
	twice := ReplaceIdentityText(once, opt)
	if once != twice {
		t.Fatalf("not idempotent: %q -> %q", once, twice)
	}
}

func TestIdentityPrefix(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI tool."
	out := ReplaceIdentityText(in, identityOpts(map[string]string{"Claude Code": "WorkBuddy"}))
	want := "You are WorkBuddy, Anthropic's official CLI tool."
	if out != want {
		t.Fatalf("prefix not rewritten:\n got %q\nwant %q", out, want)
	}
}

func TestIdentityWholeWord(t *testing.T) {
	in := "Claude Claudius Claude-Code `Claude`"
	out := ReplaceIdentityText(in, identityOpts(map[string]string{"Claude": "WorkBuddy"}))
	want := "WorkBuddy Claudius WorkBuddy-Code `Claude`"
	if out != want {
		t.Fatalf("whole-word replacement wrong:\n got %q\nwant %q", out, want)
	}
}

func TestIdentityLongestKeyWins(t *testing.T) {
	in := "Claude Code"
	out := ReplaceIdentityText(in, identityOpts(map[string]string{"Claude": "A", "Claude Code": "WorkBuddy"}))
	if out != "WorkBuddy" {
		t.Fatalf("longest key did not win: %q", out)
	}
}

func TestIdentityDisabledNoop(t *testing.T) {
	in := "You are Codex"
	if out := ReplaceIdentityText(in, IdentityOptions{Enabled: false, Target: "WorkBuddy", Replacements: map[string]string{"Codex": "WorkBuddy"}}); out != in {
		t.Fatalf("disabled changed text: %q", out)
	}
	body := []byte(`{"messages":[{"role":"system","content":"You are Codex"}]}`)
	if out := ApplyIdentity(body, IdentityOptions{Enabled: false, Target: "WorkBuddy", Replacements: map[string]string{"Codex": "WorkBuddy"}}); string(out) != string(body) {
		t.Fatalf("disabled changed body: %s", out)
	}
}

func TestIdentitySkipsKeyEqualToTarget(t *testing.T) {
	in := "You are Codex"
	out := ReplaceIdentityText(in, identityOpts(map[string]string{"Codex": "Codex"}))
	if out != in {
		t.Fatalf("key==target must be skipped: %q", out)
	}
}

func TestClientIdentitySnapshot(t *testing.T) {
	var c Client
	if got := c.IdentitySnapshot(); got.Enabled || got.Target != "" || got.Replacements != nil {
		t.Fatalf("unset client snapshot must be zero, got %#v", got)
	}
	opt := identityOpts(map[string]string{"Codex": "WorkBuddy"})
	c.SetIdentity(opt)
	got := c.IdentitySnapshot()
	if !got.Enabled || got.Target != "WorkBuddy" || got.Replacements["Codex"] != "WorkBuddy" {
		t.Fatalf("snapshot mismatch: %#v", got)
	}
	opt.Replacements["Claude"] = "WorkBuddy"
	if _, ok := c.IdentitySnapshot().Replacements["Claude"]; ok {
		t.Fatal("SetIdentity must store a copy of replacements")
	}
	got.Replacements["X"] = "Y"
	if _, ok := c.IdentitySnapshot().Replacements["X"]; ok {
		t.Fatal("IdentitySnapshot must return a copy of replacements")
	}
}

func TestIdentityChannelCompatPhrases(t *testing.T) {
	opt := IdentityOptions{
		Enabled: true, Target: "WorkBuddy",
		Replacements: map[string]string{
			"workspace root":     "workspace base",
			"are paths the user": "are paths that the user",
		},
	}
	in := "The workspace root is here. Files are paths the user can edit."
	out := ReplaceIdentityText(in, opt)
	if !strings.Contains(out, "workspace base") || !strings.Contains(out, "are paths that the user") {
		t.Fatalf("channel compat replacements missing: %q", out)
	}
}
