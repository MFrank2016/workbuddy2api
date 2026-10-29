package server

import (
	"encoding/json"
	"testing"
)

func TestResponsesUsageMapping(t *testing.T) {
	got := responsesUsage(map[string]any{
		"prompt_tokens": float64(3), "completion_tokens": float64(2), "total_tokens": float64(5),
	})
	if got["input_tokens"].(int) != 3 || got["output_tokens"].(int) != 2 {
		t.Fatalf("usage=%v", got)
	}
	if got["total_tokens"].(int) != 5 {
		t.Fatalf("total_tokens=%v", got["total_tokens"])
	}
	if got["input_tokens_details"].(map[string]any)["cached_tokens"].(int) != 0 {
		t.Fatalf("cached_tokens=%v", got["input_tokens_details"])
	}
}

func TestResponsesUsageCachedAndFallbackTotal(t *testing.T) {
	got := responsesUsage(map[string]any{
		"prompt_tokens":         float64(4),
		"completion_tokens":     float64(6),
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(2)},
	})
	if got["total_tokens"].(int) != 10 {
		t.Fatalf("total_tokens=%v want 10", got["total_tokens"])
	}
	if got["input_tokens_details"].(map[string]any)["cached_tokens"].(int) != 2 {
		t.Fatalf("cached_tokens=%v want 2", got["input_tokens_details"])
	}
}

func TestToolCallsParse(t *testing.T) {
	msg := map[string]any{"tool_calls": []any{map[string]any{
		"id": "c1", "function": map[string]any{"name": "get_weather", "arguments": "{}"},
	}}}
	got := toolCalls(msg)
	if len(got) != 1 || got[0].ID != "c1" || got[0].Name != "get_weather" {
		t.Fatalf("got=%v", got)
	}
	if got[0].Arguments != "{}" {
		t.Fatalf("arguments=%q", got[0].Arguments)
	}
}

func TestToolCallDeltasParse(t *testing.T) {
	delta := map[string]any{"tool_calls": []any{map[string]any{
		"index": float64(0), "id": "c2", "function": map[string]any{"name": "lookup", "arguments": `{"q":`},
	}}}
	got := toolCallDeltas(delta)
	if len(got) != 1 || got[0].ID != "c2" || got[0].Name != "lookup" || got[0].Arguments != `{"q":` {
		t.Fatalf("got=%v", got)
	}
}

func TestFirstChoiceMessageAndDelta(t *testing.T) {
	msg := firstChoiceMessage(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": "hi"},
	}}})
	if msg == nil || msg["content"] != "hi" {
		t.Fatalf("msg=%v", msg)
	}
	if firstChoiceMessage(map[string]any{}) != nil {
		t.Fatal("expected nil message for empty choices")
	}

	delta := firstDelta(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": "tok"},
	}}})
	if delta == nil || delta["content"] != "tok" {
		t.Fatalf("delta=%v", delta)
	}
	if firstDelta(map[string]any{}) != nil {
		t.Fatal("expected nil delta for empty choices")
	}
}

func TestUsageInt(t *testing.T) {
	chat := map[string]any{"usage": map[string]any{"prompt_tokens": float64(7), "completion_tokens": int64(3)}}
	if usageInt(chat, "prompt_tokens") != 7 || usageInt(chat, "completion_tokens") != 3 {
		t.Fatalf("usageInt mismatch")
	}
	if usageInt(map[string]any{}, "prompt_tokens") != 0 {
		t.Fatal("expected zero for missing usage")
	}
}

func TestPeekStream(t *testing.T) {
	if !peekStream([]byte(`{"model":"m","stream":true}`)) {
		t.Fatal("expected stream=true")
	}
	if peekStream([]byte(`{"model":"m"}`)) {
		t.Fatal("expected stream=false")
	}
	if peekStream([]byte(`not json`)) {
		t.Fatal("expected stream=false for invalid json")
	}
}

func TestParseResponsesInputString(t *testing.T) {
	items, err := parseResponsesInput(json.RawMessage(`"hello"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	msg := items[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "hello" {
		t.Fatalf("msg=%v", msg)
	}
}

func TestParseResponsesInputMessage(t *testing.T) {
	items, err := parseResponsesInput(json.RawMessage(`[
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"message","role":"assistant","content":"ok"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items=%d want 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("role=%v", first["role"])
	}
	parts, ok := first["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content=%v", first["content"])
	}
	part := parts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "hi" {
		t.Fatalf("part=%v", part)
	}
	second := items[1].(map[string]any)
	if second["role"] != "assistant" || second["content"] != "ok" {
		t.Fatalf("second=%v", second)
	}
}

func TestParseResponsesInputFunctionCallAndOutput(t *testing.T) {
	items, err := parseResponsesInput(json.RawMessage(`[
		{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
		{"type":"function_call_output","call_id":"c1","output":"sunny"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items=%d want 2", len(items))
	}
	assistant := items[0].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("role=%v", assistant["role"])
	}
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%v", assistant["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "c1" || call["type"] != "function" {
		t.Fatalf("call=%v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"SF"}` {
		t.Fatalf("function=%v", fn)
	}
	tool := items[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != "sunny" {
		t.Fatalf("tool=%v", tool)
	}
}

func TestParseResponsesInputReasoning(t *testing.T) {
	items, err := parseResponsesInput(json.RawMessage(`[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	first := items[0].(map[string]any)
	if first["role"] != "assistant" || first["reasoning_content"] != "thinking" || first["content"] != "" {
		t.Fatalf("first=%v", first)
	}

	items, err = parseResponsesInput(json.RawMessage(`[
		{"type":"reasoning","summary":"plain"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	second := items[0].(map[string]any)
	if second["role"] != "assistant" || second["reasoning_content"] != "plain" {
		t.Fatalf("second=%v", second)
	}
}

func TestParseResponsesInputWebSearchSkipped(t *testing.T) {
	items, err := parseResponsesInput(json.RawMessage(`[
		{"type":"web_search_call","id":"ws1","status":"completed"},
		{"type":"message","role":"user","content":"hi"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	msg := items[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "hi" {
		t.Fatalf("msg=%v", msg)
	}
}
