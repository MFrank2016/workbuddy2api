package server

import (
	"strings"
	"testing"
)

func anthropicTestChat(finishReason string, message map[string]any) map[string]any {
	chat := map[string]any{
		"id": "chatcmpl-1", "model": "claude-sonnet",
		"choices": []any{map[string]any{"message": message, "finish_reason": finishReason}},
		"usage":   map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(2)},
	}
	return chat
}

func TestBuildAnthropicMessage(t *testing.T) {
	chat := anthropicTestChat("tool_calls", map[string]any{
		"role": "assistant", "content": "hello",
		"tool_calls": []any{map[string]any{
			"id": "toolu_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": `{"city":"SF"}`},
		}},
	})
	out := buildAnthropicMessage(chat, &anthropicMeta{Model: "claude-sonnet"})
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("out=%v", out)
	}
	if out["model"] != "claude-sonnet" || out["stop_reason"] != "tool_use" {
		t.Fatalf("out=%v", out)
	}
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "msg_") || len(id) != len("msg_")+24 {
		t.Fatalf("id=%v want msg_ + 24 hex chars", out["id"])
	}
	if out["stop_sequence"] != nil {
		t.Fatalf("stop_sequence=%v want nil", out["stop_sequence"])
	}
	content := out["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content=%v", content)
	}
	if content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("text block=%v", content[0])
	}
	tool := content[1].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "toolu_1" || tool["name"] != "get_weather" {
		t.Fatalf("tool block=%v", tool)
	}
	input, _ := tool["input"].(map[string]any)
	if input["city"] != "SF" {
		t.Fatalf("tool input=%v", tool["input"])
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 3 || usage["output_tokens"] != 2 {
		t.Fatalf("usage=%v", usage)
	}
}

func TestBuildAnthropicMessageContentOrder(t *testing.T) {
	chat := anthropicTestChat("tool_calls", map[string]any{
		"role": "assistant", "reasoning_content": "let me think", "content": "answer",
		"tool_calls": []any{map[string]any{
			"id": "toolu_2", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`},
		}},
	})
	out := buildAnthropicMessage(chat, &anthropicMeta{Model: "claude-sonnet"})
	content := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content=%v want thinking+text+tool_use", content)
	}
	want := []string{"thinking", "text", "tool_use"}
	for i, typ := range want {
		if got := content[i].(map[string]any)["type"]; got != typ {
			t.Fatalf("content[%d]=%v want %s", i, content[i], typ)
		}
	}
	thinking := content[0].(map[string]any)
	if thinking["thinking"] != "let me think" {
		t.Fatalf("thinking block=%v", thinking)
	}
}

func TestBuildAnthropicMessageStopReasonMapping(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"stop_sequence", "stop_sequence"},
		{"", "end_turn"},
		{"unknown", "end_turn"},
	}
	for _, tc := range cases {
		t.Run(tc.finish, func(t *testing.T) {
			chat := anthropicTestChat(tc.finish, map[string]any{"role": "assistant", "content": "hi"})
			out := buildAnthropicMessage(chat, &anthropicMeta{Model: "claude-sonnet"})
			if out["stop_reason"] != tc.want {
				t.Fatalf("finish=%q stop_reason=%v want %s", tc.finish, out["stop_reason"], tc.want)
			}
		})
	}
}

func TestBuildAnthropicMessageInvalidToolArguments(t *testing.T) {
	chat := anthropicTestChat("tool_calls", map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id": "toolu_3", "type": "function",
			"function": map[string]any{"name": "broken", "arguments": `{not json`},
		}},
	})
	out := buildAnthropicMessage(chat, &anthropicMeta{Model: "claude-sonnet"})
	content := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	tool := content[0].(map[string]any)
	input, ok := tool["input"].(map[string]any)
	if !ok || len(input) != 0 {
		t.Fatalf("input=%v want empty object on parse failure", tool["input"])
	}
}

func TestBuildAnthropicMessageEmpty(t *testing.T) {
	out := buildAnthropicMessage(map[string]any{}, &anthropicMeta{Model: "claude-sonnet"})
	content := out["content"].([]any)
	if len(content) != 0 {
		t.Fatalf("content=%v want empty", content)
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason=%v", out["stop_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 0 || usage["output_tokens"] != 0 {
		t.Fatalf("usage=%v", usage)
	}
}
