package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactJSON(t *testing.T) {
	in := map[string]any{
		"authorization": "Bearer secret",
		"access_token":  "at-123",
		"nested":        map[string]any{"refresh_token": "rt-456", "safe": "ok"},
	}
	out := RedactJSON(in).(map[string]any)
	if out["authorization"] == "Bearer secret" {
		t.Fatal("authorization not redacted")
	}
	if out["access_token"] == "at-123" {
		t.Fatal("access_token not redacted")
	}
	nested := out["nested"].(map[string]any)
	if nested["refresh_token"] == "rt-456" {
		t.Fatal("refresh_token not redacted")
	}
	if nested["safe"] != "ok" {
		t.Fatal("safe field changed")
	}
}

func TestDebugCaptureWritesRedactedJSONL(t *testing.T) {
	dir := t.TempDir()
	d := &DebugCapture{Enabled: true, FullRequests: true, Dir: dir}
	d.Capture("full_request_inbound", map[string]any{
		"path": "/v1/chat/completions",
		"body": map[string]any{"authorization": "Bearer secret", "messages": []any{}},
	})
	files, err := filepath.Glob(filepath.Join(dir, "debug-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Bearer secret") {
		t.Fatalf("secret leaked: %s", raw)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["event"] != "full_request_inbound" {
		t.Fatalf("event=%v", rec["event"])
	}
}

func TestDebugCaptureDisabledNoFile(t *testing.T) {
	dir := t.TempDir()
	d := &DebugCapture{Enabled: false, Dir: dir}
	d.Capture("x", map[string]any{"a": 1})
	files, _ := filepath.Glob(filepath.Join(dir, "debug-*.jsonl"))
	if len(files) != 0 {
		t.Fatalf("disabled capture wrote files: %v", files)
	}
}
