package server

import (
	"encoding/json"
	"testing"
)

func TestResponsesRequestTranslation(t *testing.T) {
	raw := []byte(`{
	  "model":"glm-5.2","stream":true,"instructions":"be terse",
	  "input":[
	    {"role":"user","content":[{"type":"input_text","text":"hi"}]},
	    {"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
	    {"type":"function_call_output","call_id":"c1","output":"sunny"}
	  ],
	  "tools":[{"type":"function","name":"get_weather","description":"weather","parameters":{"type":"object"}}],
	  "max_output_tokens":256,"reasoning":{"effort":"high","summary":"auto"}
	}`)
	body, meta, err := buildChatBodyFromResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Stream || meta.Model != "glm-5.2" {
		t.Fatalf("meta=%+v", meta)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["max_tokens"].(float64) != 256 {
		t.Fatalf("max_tokens=%v", got["max_tokens"])
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("effort=%v", got["reasoning_effort"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4 (system,user,assistant tool_call,tool)", len(msgs))
	}
}

func TestResponsesRequestAbsentInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"absent", `{"model":"glm-5.2","instructions":"be terse"}`},
		{"null", `{"model":"glm-5.2","instructions":"be terse","input":null}`},
		{"empty", `{"model":"glm-5.2","instructions":"be terse","input":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := buildChatBodyFromResponses([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			msgs, ok := got["messages"].([]any)
			if !ok {
				t.Fatalf("messages=%v", got["messages"])
			}
			if len(msgs) != 1 {
				t.Fatalf("messages=%d want 1 (system only)", len(msgs))
			}
			if msgs[0].(map[string]any)["role"] != "system" {
				t.Fatalf("message=%v", msgs[0])
			}
			if got["stream"] != true {
				t.Fatalf("stream=%v", got["stream"])
			}
		})
	}
}

func TestResponsesRequestFunctionTool(t *testing.T) {
	raw := []byte(`{
	  "model":"glm-5.2","input":"weather?",
	  "tools":[{"type":"function","name":"get_weather","description":"weather","parameters":{"type":"object"}}]
	}`)
	body, meta, err := buildChatBodyFromResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools=%v", got["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool type=%v", tool["type"])
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "weather" {
		t.Fatalf("function=%v", fn)
	}
	if params, ok := fn["parameters"].(map[string]any); !ok || params["type"] != "object" {
		t.Fatalf("parameters=%v", fn["parameters"])
	}
	if meta.ToolKinds["get_weather"] != "function" {
		t.Fatalf("meta.ToolKinds=%v", meta.ToolKinds)
	}
}

func TestResponsesRequestOptionalMappings(t *testing.T) {
	raw := []byte(`{
	  "model":"glm-5.2","input":"hi",
	  "reasoning":{"effort":"low","summary":"concise"},
	  "text":{"verbosity":"low"},
	  "parallel_tool_calls":false,
	  "tool_choice":{"type":"function","name":"get_weather"}
	}`)
	body, _, err := buildChatBodyFromResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["reasoning_effort"] != "low" || got["reasoning_summary"] != "concise" {
		t.Fatalf("reasoning fields=%v/%v", got["reasoning_effort"], got["reasoning_summary"])
	}
	if got["verbosity"] != "low" {
		t.Fatalf("verbosity=%v", got["verbosity"])
	}
	if got["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls=%v", got["parallel_tool_calls"])
	}
	choice, ok := got["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "function" {
		t.Fatalf("tool_choice=%v", got["tool_choice"])
	}
	fn := choice["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool_choice function=%v", fn)
	}
}
