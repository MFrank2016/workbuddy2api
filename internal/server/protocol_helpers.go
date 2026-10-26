package server

import (
	"encoding/json"
	"net/http"
)

type responsesTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
	Format      any    `json:"format"`
}

type chatToolCall struct {
	ID        string
	Name      string
	Arguments string
}

func parseResponsesInput(raw json.RawMessage) ([]any, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := []any{}
	for _, item := range items {
		typ, _ := item["type"].(string)
		switch typ {
		case "message", "":
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			out = append(out, map[string]any{"role": role, "content": responsesContentToChat(item["content"])})
		case "function_call":
			call := map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}}
			if len(out) > 0 {
				if last, ok := out[len(out)-1].(map[string]any); ok && last["role"] == "assistant" {
					last["tool_calls"] = appendAny(last["tool_calls"], call)
					continue
				}
			}
			out = append(out, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}})
		case "function_call_output":
			out = append(out, map[string]any{"role": "tool", "tool_call_id": item["call_id"], "content": item["output"]})
		case "reasoning":
			text := responsesReasoningText(item)
			if len(out) > 0 {
				if last, ok := out[len(out)-1].(map[string]any); ok && last["role"] == "assistant" {
					last["reasoning_content"] = text
					continue
				}
			}
			out = append(out, map[string]any{"role": "assistant", "content": "", "reasoning_content": text})
		case "web_search_call":
			continue
		}
	}
	return out, nil
}

func responsesContentToChat(v any) any {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := make([]any, 0, len(x))
		for _, p := range x {
			m, _ := p.(map[string]any)
			if m == nil {
				continue
			}
			if t, _ := m["text"].(string); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		}
		return parts
	default:
		return ""
	}
}

func responsesReasoningText(item map[string]any) string {
	if s, _ := item["summary"].(string); s != "" {
		return s
	}
	if arr, _ := item["summary"].([]any); len(arr) > 0 {
		if m, _ := arr[0].(map[string]any); m != nil {
			s, _ := m["text"].(string)
			return s
		}
	}
	return ""
}

func appendAny(v any, x any) []any {
	arr, _ := v.([]any)
	return append(arr, x)
}

func firstChoiceMessage(chat map[string]any) map[string]any {
	chs, _ := chat["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	return msg
}

func firstDelta(chunk map[string]any) map[string]any {
	chs, _ := chunk["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	d, _ := c["delta"].(map[string]any)
	return d
}

func toolCalls(msg map[string]any) []chatToolCall {
	raw, _ := msg["tool_calls"].([]any)
	out := make([]chatToolCall, 0, len(raw))
	for _, x := range raw {
		tc, _ := x.(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		id, _ := tc["id"].(string)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		out = append(out, chatToolCall{ID: id, Name: name, Arguments: args})
	}
	return out
}

func toolCallDeltas(delta map[string]any) []chatToolCall {
	raw, _ := delta["tool_calls"].([]any)
	out := make([]chatToolCall, 0, len(raw))
	for _, x := range raw {
		tc, _ := x.(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		id, _ := tc["id"].(string)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		out = append(out, chatToolCall{ID: id, Name: name, Arguments: args})
	}
	return out
}

func responsesUsage(v any) map[string]any {
	m, _ := v.(map[string]any)
	in := intFromAny(m["prompt_tokens"])
	out := intFromAny(m["completion_tokens"])
	total := intFromAny(m["total_tokens"])
	if total == 0 {
		total = in + out
	}
	details, _ := m["prompt_tokens_details"].(map[string]any)
	cached := 0
	if details != nil {
		cached = intFromAny(details["cached_tokens"])
	}
	return map[string]any{
		"input_tokens": in, "output_tokens": out, "total_tokens": total,
		"input_tokens_details": map[string]any{"cached_tokens": cached},
	}
}

func usageInt(chat map[string]any, key string) int {
	m, _ := chat["usage"].(map[string]any)
	return intFromAny(m[key])
}

func peekStream(body []byte) bool {
	var p struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Stream
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	default:
		return 0
	}
}

func writeResponsesError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code}})
}

func writeResponsesErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	e := map[string]any{"message": msg, "type": "invalid_request_error", "code": code}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	writeJSON(w, status, map[string]any{"error": e})
}
