package server

import "testing"

func TestBuildResponsesObject(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-1", "model": "glm-5.2", "created": float64(1700000000),
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": "hello", "reasoning_content": "think",
		}, "finish_reason": "stop"}},
		"usage": map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(2), "total_tokens": float64(5)},
	}
	out := buildResponsesObject(chat, &responsesMeta{Model: "glm-5.2"})
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("out=%v", out)
	}
	items := out["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output items=%d want reasoning+message", len(items))
	}
	if items[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("first item=%v want reasoning", items[0])
	}
	if items[1].(map[string]any)["type"] != "message" {
		t.Fatalf("second item=%v want message", items[1])
	}
}

func TestBuildResponsesObjectToolCalls(t *testing.T) {
	chat := map[string]any{
		"created": float64(1700000000),
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": "call_abc",
				"function": map[string]any{
					"name":      "get_weather",
					"arguments": `{"city":"Paris"}`,
				},
			}},
		}, "finish_reason": "tool_calls"}},
		"usage": map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(2)},
	}
	out := buildResponsesObject(chat, &responsesMeta{Model: "glm-5.2"})
	items := out["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output items=%d want 1 function_call", len(items))
	}
	fc := items[0].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_abc" || fc["name"] != "get_weather" {
		t.Fatalf("function_call=%v", fc)
	}
	if fc["arguments"] != `{"city":"Paris"}` || fc["status"] != "completed" {
		t.Fatalf("function_call=%v", fc)
	}
	if id, _ := fc["id"].(string); len(id) < 4 || id[:3] != "fc_" {
		t.Fatalf("function_call id=%v want fc_ prefix", fc["id"])
	}
}
