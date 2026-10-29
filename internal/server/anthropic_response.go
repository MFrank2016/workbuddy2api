package server

import "encoding/json"

// buildAnthropicMessage 把 Chat completions 聚合结果翻译成 Anthropic message。
func buildAnthropicMessage(chat map[string]any, meta *anthropicMeta) map[string]any {
	content := []any{}
	stop := anthropicStopReason(firstFinishReason(chat))
	if msg := firstChoiceMessage(chat); msg != nil {
		if rc, _ := msg["reasoning_content"].(string); rc != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": rc})
		}
		if text, _ := msg["content"].(string); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		calls := toolCalls(msg)
		for _, tc := range calls {
			input := map[string]any{}
			_ = json.Unmarshal([]byte(tc.Arguments), &input)
			content = append(content, map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": input,
			})
		}
		if len(calls) > 0 && stop == "end_turn" {
			stop = "tool_use"
		}
	}
	return map[string]any{
		"id": "msg_" + randomHex(12), "type": "message", "role": "assistant", "model": meta.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": usageInt(chat, "prompt_tokens"), "output_tokens": usageInt(chat, "completion_tokens"),
		},
	}
}

// firstFinishReason 返回首个 choice 的 finish_reason。
func firstFinishReason(chat map[string]any) string {
	chs, _ := chat["choices"].([]any)
	if len(chs) == 0 {
		return ""
	}
	c, _ := chs[0].(map[string]any)
	reason, _ := c["finish_reason"].(string)
	return reason
}

// anthropicStopReason 把 Chat finish_reason 映射成 Anthropic stop_reason。
func anthropicStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}
