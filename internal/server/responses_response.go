package server

func buildResponsesObject(chat map[string]any, meta *responsesMeta) map[string]any {
	id := "resp_" + randomHex(12)
	output := []any{}
	if msg := firstChoiceMessage(chat); msg != nil {
		if rc, _ := msg["reasoning_content"].(string); rc != "" {
			output = append(output, map[string]any{
				"type": "reasoning", "id": "rs_" + randomHex(8),
				"summary": []any{map[string]any{"type": "summary_text", "text": rc}},
			})
		}
		if content, _ := msg["content"].(string); content != "" {
			output = append(output, map[string]any{
				"type": "message", "id": "msg_" + randomHex(8), "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
			})
		}
		for _, tc := range toolCalls(msg) {
			output = append(output, map[string]any{
				"type": "function_call", "id": "fc_" + randomHex(8), "call_id": tc.ID,
				"name": tc.Name, "arguments": tc.Arguments, "status": "completed",
			})
		}
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": chat["created"],
		"status": "completed", "model": meta.Model,
		"output": output, "usage": responsesUsage(chat["usage"]),
	}
}
