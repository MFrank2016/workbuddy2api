package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func dshStringContent(t *testing.T, body []byte, idx int) string {
	t.Helper()
	var root struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if idx >= len(root.Messages) {
		t.Fatalf("message %d missing", idx)
	}
	var s string
	if err := json.Unmarshal(root.Messages[idx].Content, &s); err != nil {
		t.Fatalf("message %d content not a string: %v", idx, err)
	}
	return s
}

func TestDSHGUIContextNormalized(t *testing.T) {
	body := []byte(`{"messages":[
	  {"role":"system","content":"You are an AI agent powered by DeepSeek Harness.\nYou are operating in the DSH GUI environment with window controls."},
	  {"role":"user","content":"keep this DSH GUI environment sentence"}
	]}`)
	out := ApplyDSHCompatibility(body, "WorkBuddy")
	sys := dshStringContent(t, out, 0)
	if strings.Contains(sys, "DeepSeek Harness") || strings.Contains(sys, "DSH GUI environment") {
		t.Fatalf("system not normalized: %s", sys)
	}
	if !strings.Contains(sys, "You are WorkBuddy.") {
		t.Fatalf("identity sentence not normalized: %s", sys)
	}
	if !strings.Contains(sys, dshTextInterface) {
		t.Fatalf("GUI sentence not normalized: %s", sys)
	}
	if got := dshStringContent(t, out, 1); got != "keep this DSH GUI environment sentence" {
		t.Fatalf("user changed: %s", got)
	}
}

func TestDSHTextBlocksAndRoles(t *testing.T) {
	body := []byte(`{"messages":[
	  {"role":"system","content":[{"type":"text","text":"You are operating in the DSH GUI environment"},{"type":"reasoning","text":"You are operating in the DSH GUI environment"}]},
	  {"role":"assistant","content":[{"type":"text","text":"You are an AI agent powered by DeepSeek Harness"}]},
	  {"role":"developer","content":"DSH GUI environment note"},
	  {"role":"user","content":[{"type":"text","text":"You are an AI agent powered by DeepSeek Harness"}]},
	  {"role":"tool","content":"DSH GUI environment output"}
	]}`)
	out := ApplyDSHCompatibility(body, "Copilot")
	var root struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	var systemParts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(root.Messages[0].Content, &systemParts); err != nil {
		t.Fatalf("invalid system parts: %v", err)
	}
	if systemParts[0].Text != dshTextInterface {
		t.Fatalf("system text block not normalized: %q", systemParts[0].Text)
	}
	if systemParts[1].Text != "You are operating in the DSH GUI environment" {
		t.Fatalf("reasoning block changed: %q", systemParts[1].Text)
	}
	var assistantParts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(root.Messages[1].Content, &assistantParts); err != nil {
		t.Fatalf("invalid assistant parts: %v", err)
	}
	if assistantParts[0].Text != "You are Copilot" {
		t.Fatalf("assistant text block not normalized: %q", assistantParts[0].Text)
	}
	if got := dshStringContent(t, out, 2); got != "text interface note" {
		t.Fatalf("developer not normalized: %q", got)
	}
	var userParts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(root.Messages[3].Content, &userParts); err != nil {
		t.Fatalf("invalid user parts: %v", err)
	}
	if userParts[0].Text != "You are an AI agent powered by DeepSeek Harness" {
		t.Fatalf("user changed: %q", userParts[0].Text)
	}
	if got := dshStringContent(t, out, 4); got != "DSH GUI environment output" {
		t.Fatalf("tool changed: %q", got)
	}
}

func TestDSHSkipsCodeBlocks(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"` + "```" + `\nYou are an AI agent powered by DeepSeek Harness\nDSH GUI environment\n` + "```" + `\nYou are operating in the DSH GUI environment\nUse ` + "`DSH GUI environment`" + ` here.\n    DSH GUI environment"}]}`)
	out := ApplyDSHCompatibility(body, "WorkBuddy")
	sys := dshStringContent(t, out, 0)
	for _, want := range []string{
		"```\nYou are an AI agent powered by DeepSeek Harness\nDSH GUI environment\n```",
		"`DSH GUI environment`",
		"\n    DSH GUI environment",
	} {
		if !strings.Contains(sys, want) {
			t.Fatalf("code block changed, missing %q in %s", want, sys)
		}
	}
	if !strings.Contains(sys, dshTextInterface) {
		t.Fatalf("prose not normalized: %s", sys)
	}
}

func TestDSHIdempotent(t *testing.T) {
	body := []byte(`{"messages":[
	  {"role":"system","content":"You are an AI agent powered by DeepSeek Harness.\nYou are operating in the DSH GUI environment with window controls."},
	  {"role":"assistant","content":[{"type":"text","text":"DSH GUI environment"}]}
	]}`)
	once := ApplyDSHCompatibility(body, "WorkBuddy")
	twice := ApplyDSHCompatibility(once, "WorkBuddy")
	if !bytes.Equal(once, twice) {
		t.Fatalf("not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
}

func TestDSHEmptyTargetDefaultsToWorkBuddy(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness"}]}`)
	out := ApplyDSHCompatibility(body, "")
	sys := dshStringContent(t, out, 0)
	if sys != "You are WorkBuddy" {
		t.Fatalf("empty target not defaulted: %q", sys)
	}
}
