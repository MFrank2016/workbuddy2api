package server

import (
	"encoding/json"
	"testing"
)

func TestResponsesFunctionToolRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"glm-5.2","input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	body, meta, err := buildChatBodyFromResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	tools := got["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool=%v", fn)
	}
	if meta.ToolKinds["get_weather"] != "function" {
		t.Fatalf("meta=%v", meta.ToolKinds)
	}

	out := buildResponsesObject(toolCallChat("call_fn", "get_weather", `{"city":"Paris"}`), meta)
	items := out["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output items=%d want 1", len(items))
	}
	fc := items[0].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_fn" || fc["name"] != "get_weather" {
		t.Fatalf("function_call=%v", fc)
	}
	if fc["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("arguments=%v", fc["arguments"])
	}
}

func TestResponsesCustomToolRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"glm-5.2","input":"run it","tools":[{"type":"custom","name":"run_code","description":"run code"}]}`)
	body, meta, err := buildChatBodyFromResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	tools := got["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "run_code" || fn["description"] != "run code" {
		t.Fatalf("tool=%v", fn)
	}
	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("parameters=%v", params)
	}
	if meta.ToolKinds["run_code"] != "custom" {
		t.Fatalf("meta=%v", meta.ToolKinds)
	}

	out := buildResponsesObject(toolCallChat("call_custom", "run_code", `{"input":"print(1)"}`), meta)
	items := out["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output items=%d want 1", len(items))
	}
	ctc := items[0].(map[string]any)
	if ctc["type"] != "custom_tool_call" || ctc["call_id"] != "call_custom" || ctc["name"] != "run_code" {
		t.Fatalf("custom_tool_call=%v", ctc)
	}
	if ctc["input"] != "print(1)" {
		t.Fatalf("input=%v", ctc["input"])
	}
	if id, _ := ctc["id"].(string); len(id) < 4 || id[:4] != "ctc_" {
		t.Fatalf("custom_tool_call id=%v want ctc_ prefix", ctc["id"])
	}
}

func TestResponsesWebSearchToolRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"glm-5.2","input":"news?","tools":[{"type":"web_search"}]}`)
	body, meta, err := buildChatBodyFromResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	tools := got["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "web_search" || fn["description"] != "Search the web" {
		t.Fatalf("tool=%v", fn)
	}
	if meta.ToolKinds["web_search"] != "web_search" {
		t.Fatalf("meta=%v", meta.ToolKinds)
	}

	out := buildResponsesObject(toolCallChat("call_web", "web_search", `{"query":"golang"}`), meta)
	items := out["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output items=%d want 1", len(items))
	}
	fc := items[0].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_web" || fc["name"] != "web_search" {
		t.Fatalf("function_call=%v", fc)
	}
	if fc["arguments"] != `{"query":"golang"}` {
		t.Fatalf("arguments=%v", fc["arguments"])
	}
}

func TestResponsesCustomToolInputFallsBackToRawArguments(t *testing.T) {
	out := buildResponsesObject(
		toolCallChat("call_raw", "run_code", "not-json"),
		&responsesMeta{Model: "glm-5.2", ToolKinds: map[string]string{"run_code": "custom"}},
	)
	items := out["output"].([]any)
	ctc := items[0].(map[string]any)
	if ctc["input"] != "not-json" {
		t.Fatalf("input=%v want raw arguments", ctc["input"])
	}
}

func toolCallChat(callID, name, arguments string) map[string]any {
	return map[string]any{
		"created": float64(1700000000),
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": callID,
				"function": map[string]any{
					"name":      name,
					"arguments": arguments,
				},
			}},
		}, "finish_reason": "tool_calls"}},
		"usage": map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(2)},
	}
}
