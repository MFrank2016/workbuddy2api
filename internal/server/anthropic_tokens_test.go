package server

import (
	"encoding/json"
	"testing"
)

func TestEstimateAnthropicTokens(t *testing.T) {
	req := anthropicRequest{
		System: json.RawMessage(`"hello"`),
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"你好世界"}]`)},
		},
	}
	n := estimateAnthropicTokens(req)
	if n < 4 {
		t.Fatalf("tokens=%d want >=4", n)
	}
}

func TestEstimateAnthropicTokensTools(t *testing.T) {
	without := anthropicRequest{
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	with := without
	with.Tools = []anthropicTool{{
		Name:        "get_weather",
		Description: "weather",
		InputSchema: map[string]any{"type": "object"},
	}}
	if got, want := estimateAnthropicTokens(with), estimateAnthropicTokens(without); got <= want {
		t.Fatalf("tools should add tokens: got=%d without=%d", got, want)
	}
}
