package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustAnthropicBody(t *testing.T, raw string) (map[string]any, *anthropicMeta) {
	t.Helper()
	body, meta, err := buildChatBodyFromAnthropic([]byte(raw))
	if err != nil {
		t.Fatalf("buildChatBodyFromAnthropic: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal chat body: %v", err)
	}
	return got, meta
}

func anthropicMessages(t *testing.T, got map[string]any) []any {
	t.Helper()
	msgs, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("messages=%v", got["messages"])
	}
	return msgs
}

func anthropicMessageMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("message is not an object: %v", v)
	}
	return m
}

func TestAnthropicRequestTranslation(t *testing.T) {
	raw := `{
	  "model":"claude-sonnet","max_tokens":512,"system":"be terse","stream":true,
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"hi"}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}
	  ],
	  "tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object"}}],
	  "tool_choice":{"type":"auto"}
	}`
	got, meta := mustAnthropicBody(t, raw)
	if !meta.Stream || meta.Model != "claude-sonnet" {
		t.Fatalf("meta=%+v", meta)
	}
	if got["max_tokens"].(float64) != 512 {
		t.Fatalf("max_tokens=%v", got["max_tokens"])
	}
	if got["stream"] != true {
		t.Fatalf("stream=%v", got["stream"])
	}
	msgs := anthropicMessages(t, got)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want system+3", len(msgs))
	}
	sys := anthropicMessageMap(t, msgs[0])
	if sys["role"] != "system" || sys["content"] != "be terse" {
		t.Fatalf("system msg=%v", sys)
	}
	user := anthropicMessageMap(t, msgs[1])
	if user["role"] != "user" || user["content"] != "hi" {
		t.Fatalf("user msg=%v", user)
	}
	asst := anthropicMessageMap(t, msgs[2])
	calls, ok := asst["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls missing: %v", asst)
	}
	call := anthropicMessageMap(t, calls[0])
	fn := anthropicMessageMap(t, call["function"])
	if call["id"] != "toolu_1" || fn["name"] != "get_weather" {
		t.Fatalf("tool call=%v", call)
	}
	if args, _ := fn["arguments"].(string); !strings.Contains(args, `"city":"SF"`) {
		t.Fatalf("arguments=%v", fn["arguments"])
	}
	tool := anthropicMessageMap(t, msgs[3])
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "sunny" {
		t.Fatalf("tool msg=%v", tool)
	}
	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice=%v", got["tool_choice"])
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools=%v", got["tools"])
	}
	toolDef := anthropicMessageMap(t, tools[0])
	if toolDef["type"] != "function" {
		t.Fatalf("tool def=%v", toolDef)
	}
	if meta.ToolKinds["get_weather"] != "function" {
		t.Fatalf("meta.ToolKinds=%v", meta.ToolKinds)
	}
}

func TestAnthropicRequestSystemArray(t *testing.T) {
	raw := `{
	  "model":"claude-sonnet","max_tokens":16,
	  "system":[{"type":"text","text":"first"},{"type":"text","text":"second"}],
	  "messages":[{"role":"user","content":"hi"}]
	}`
	got, _ := mustAnthropicBody(t, raw)
	msgs := anthropicMessages(t, got)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want system+user", len(msgs))
	}
	sys := anthropicMessageMap(t, msgs[0])
	if sys["role"] != "system" || sys["content"] != "first\nsecond" {
		t.Fatalf("system msg=%v", sys)
	}
}

func TestAnthropicRequestBase64Image(t *testing.T) {
	raw := `{
	  "model":"claude-sonnet","max_tokens":16,
	  "messages":[{"role":"user","content":[
	    {"type":"text","text":"what is this"},
	    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}
	  ]}]
	}`
	got, _ := mustAnthropicBody(t, raw)
	msgs := anthropicMessages(t, got)
	if len(msgs) != 1 {
		t.Fatalf("messages=%d want 1", len(msgs))
	}
	user := anthropicMessageMap(t, msgs[0])
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content=%v", user["content"])
	}
	img := anthropicMessageMap(t, parts[1])
	if img["type"] != "image_url" {
		t.Fatalf("image part=%v", img)
	}
	url := anthropicMessageMap(t, img["image_url"])
	if url["url"] != "data:image/png;base64,QUJD" {
		t.Fatalf("image url=%v", url["url"])
	}
}

func TestAnthropicRequestToolChoice(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want any
	}{
		{"auto", `{"type":"auto"}`, "auto"},
		{"any", `{"type":"any"}`, "required"},
		{"none", `{"type":"none"}`, "none"},
		{"tool", `{"type":"tool","name":"get_weather"}`, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "get_weather"},
		}},
		{"unknown", `{"type":"bogus"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"model":"claude-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tool_choice":` + tc.raw + `}`
			got, _ := mustAnthropicBody(t, raw)
			if tc.want == nil {
				if _, present := got["tool_choice"]; present {
					t.Fatalf("tool_choice should be omitted, got %v", got["tool_choice"])
				}
				return
			}
			if tc.name == "tool" {
				choice, ok := got["tool_choice"].(map[string]any)
				if !ok || choice["type"] != "function" {
					t.Fatalf("tool_choice=%v", got["tool_choice"])
				}
				fn := anthropicMessageMap(t, choice["function"])
				if fn["name"] != "get_weather" {
					t.Fatalf("tool_choice function=%v", fn)
				}
				return
			}
			if got["tool_choice"] != tc.want {
				t.Fatalf("tool_choice=%v want %v", got["tool_choice"], tc.want)
			}
		})
	}
}

func TestAnthropicRequestMultipleToolResults(t *testing.T) {
	raw := `{
	  "model":"claude-sonnet","max_tokens":16,
	  "messages":[{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_1","content":"one"},
	    {"type":"tool_result","tool_use_id":"toolu_2","content":[
	      {"type":"text","text":"two"},
	      {"type":"image","source":{"type":"url","url":"https://example.com/y.png"}}
	    ]}
	  ]}]
	}`
	got, _ := mustAnthropicBody(t, raw)
	msgs := anthropicMessages(t, got)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want tool1+tool2+image user", len(msgs))
	}
	first := anthropicMessageMap(t, msgs[0])
	if first["role"] != "tool" || first["tool_call_id"] != "toolu_1" || first["content"] != "one" {
		t.Fatalf("first tool msg=%v", first)
	}
	second := anthropicMessageMap(t, msgs[1])
	if second["role"] != "tool" || second["tool_call_id"] != "toolu_2" || second["content"] != "two" {
		t.Fatalf("second tool msg=%v", second)
	}
	imageMsg := anthropicMessageMap(t, msgs[2])
	if imageMsg["role"] != "user" {
		t.Fatalf("image msg=%v", imageMsg)
	}
	parts, ok := imageMsg["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("image content=%v", imageMsg["content"])
	}
	img := anthropicMessageMap(t, parts[0])
	if img["type"] != "image_url" {
		t.Fatalf("image part=%v", img)
	}
	url := anthropicMessageMap(t, img["image_url"])
	if url["url"] != "https://example.com/y.png" {
		t.Fatalf("image url=%v", url["url"])
	}
}
