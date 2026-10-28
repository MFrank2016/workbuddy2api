package upstream

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestOfficialBodyDefaults(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":100}`)
	out := string(ApplyOfficialBody(in))
	if !strings.Contains(out, `"max_tokens":100`) {
		t.Fatalf("max_completion_tokens not translated: %s", out)
	}
	if strings.Contains(out, "max_completion_tokens") {
		t.Fatalf("alias must be removed: %s", out)
	}
	if !strings.Contains(out, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("stream_options missing: %s", out)
	}
	if !strings.Contains(out, `"temperature":1`) {
		t.Fatalf("temperature default missing: %s", out)
	}
	if !strings.Contains(out, `"verbosity":"high"`) {
		t.Fatalf("verbosity default missing: %s", out)
	}
}

func TestOfficialBodyDefaultsMaxTokens(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[]}`)
	out := string(ApplyOfficialBody(in))
	if !strings.Contains(out, `"max_tokens":128000`) {
		t.Fatalf("max_tokens default missing: %s", out)
	}
}

func TestOfficialBodyExplicitValuesPreserved(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[],"max_tokens":42,` +
		`"temperature":0.3,"top_p":0.9,"verbosity":"low",` +
		`"stream_options":{"include_usage":false}}`)
	out := string(ApplyOfficialBody(in))
	for _, want := range []string{
		`"max_tokens":42`,
		`"temperature":0.3`,
		`"top_p":0.9`,
		`"verbosity":"low"`,
		`"stream_options":{"include_usage":false}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("explicit value not preserved (%s): %s", want, out)
		}
	}
	if strings.Contains(out, "128000") {
		t.Fatalf("max_tokens default must not override explicit value: %s", out)
	}
}

func TestOfficialBodyTopPNoTemperatureDefault(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[],"top_p":0.5}`)
	out := string(ApplyOfficialBody(in))
	if strings.Contains(out, `"temperature"`) {
		t.Fatalf("temperature must not default when top_p set: %s", out)
	}
}

func TestOfficialBodyOtherModelUntouchedDefaults(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	out := string(ApplyOfficialBody(in))
	if strings.Contains(out, "max_tokens") || strings.Contains(out, "verbosity") || strings.Contains(out, `"temperature"`) {
		t.Fatalf("model-specific defaults leaked to other model: %s", out)
	}
	if !strings.Contains(out, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("stream_options should be applied to all models: %s", out)
	}
}

func TestOfficialBodyInvalidJSONUnchanged(t *testing.T) {
	in := []byte(`not json`)
	if out := ApplyOfficialBody(in); !bytes.Equal(out, in) {
		t.Fatalf("invalid body must be returned unchanged: %q", out)
	}
}

func TestOfficialHeaders(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	req.Header.Set("Origin", "https://www.codebuddy.cn")
	req.Header.Set("Referer", "https://www.codebuddy.cn/")
	req.Header.Set("X-Client-ID", "legacy")
	req.Header.Set("X-Client-Version", "1.0.0")
	req.Header.Set("X-Refresh-Token", "rt")
	req.Header.Set("X-No-Authorization", "1")
	req.Header.Set("X-No-User-Id", "1")
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-No-Department-Info", "1")
	req.Header.Set("X-Conversation-Message-ID", "abc-def-123")

	c.applyOfficialHeaders(req, a)

	want := map[string]string{
		"Accept":                      "application/json",
		"User-Agent":                  "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0",
		"X-Requested-With":            "XMLHttpRequest",
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "Windows",
		"x-stainless-package-version": "6.25.0",
		"x-stainless-retry-count":     "0",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.21.1",
		"X-IDE-Type":                  "WorkBuddy",
		"X-IDE-Name":                  "WorkBuddy",
		"X-IDE-Version":               "5.8.3",
		"X-Private-Data":              "true",
		"X-Codebuddy-Request":         "1",
		"X-Agent-Intent":              "craft",
		"X-Agent-Purpose":             "conversation",
		"X-Agent-Type":                "main",
		"X-Product":                   "SaaS",
		"X-Request-ID":                "abcdef123",
	}
	for name, value := range want {
		if got := req.Header.Get(name); got != value {
			t.Errorf("%s=%q, want %q", name, got, value)
		}
	}
	for _, name := range legacyOfficialHeaders {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("legacy header %s must be removed, got %q", name, got)
		}
	}
}

func TestOfficialHeadersNonCNNoop(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}
	req := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	req.Header.Set("Origin", "https://www.workbuddy.ai")
	req.Header.Set("User-Agent", "legacy-ua")
	req.Header.Set("X-Client-ID", "legacy")

	c.applyOfficialHeaders(req, a)

	if got := req.Header.Get("User-Agent"); got != "legacy-ua" {
		t.Errorf("non-CN UA changed: %q", got)
	}
	if got := req.Header.Get("Origin"); got != "https://www.workbuddy.ai" {
		t.Errorf("non-CN Origin changed: %q", got)
	}
	if got := req.Header.Get("X-Client-ID"); got != "legacy" {
		t.Errorf("non-CN legacy header removed: %q", got)
	}
}

func TestOfficialHeadersOtherPathNoop(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	req := httptest.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/report", nil)
	req.Header.Set("User-Agent", "legacy-ua")

	c.applyOfficialHeaders(req, a)

	if got := req.Header.Get("User-Agent"); got != "legacy-ua" {
		t.Errorf("non-chat path UA changed: %q", got)
	}
}

func TestOfficialGzipBytesRoundTrip(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	out, err := gzipBytes(in)
	if err != nil {
		t.Fatalf("gzipBytes: %v", err)
	}
	if bytes.Equal(out, in) {
		t.Fatal("gzipBytes did not compress")
	}
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("round-trip mismatch: %q != %q", got, in)
	}
}

func TestOfficialBodyKeepsMessages(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	out := ApplyOfficialBody(in)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages mangled: %s", out)
	}
}
