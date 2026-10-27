package server

import (
	"encoding/json"
	"strings"
)

// buildChatBodyFromAnthropic 把 Anthropic Messages 请求翻译成 Chat completions body。
func buildChatBodyFromAnthropic(raw []byte) ([]byte, *anthropicMeta, error) {
	var in anthropicRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, nil, err
	}
	meta := &anthropicMeta{Model: in.Model, Stream: in.Stream, ToolKinds: map[string]string{}}

	msgs := []any{}
	if sys := anthropicSystemText(in.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range in.Messages {
		converted, err := anthropicMessageToChat(m)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, converted...)
	}

	body := map[string]any{"model": in.Model, "messages": msgs, "stream": true, "max_tokens": in.MaxTokens}
	if in.Temperature != nil {
		body["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		body["top_p"] = *in.TopP
	}
	if len(in.Stop) > 0 {
		body["stop"] = in.Stop
	}
	if len(in.Tools) > 0 {
		body["tools"] = anthropicToolsToChat(in.Tools, meta)
	}
	if len(in.ToolChoice) > 0 {
		if choice := anthropicToolChoice(in.ToolChoice, meta); choice != nil {
			body["tool_choice"] = choice
		}
	}

	out, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return out, meta, nil
}

// anthropicSystemText 支持字符串或 [{type:"text",text}] 数组。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// anthropicContentText 支持字符串或 text block 数组，供 count_tokens 等复用。
func anthropicContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// anthropicMessageToChat 把一条 Anthropic 消息翻译成一条或多条 Chat 消息。
// 返回多条是为了 tool_result（可能多个）与 tool_result 内图片的提升。
func anthropicMessageToChat(m anthropicMessage) ([]any, error) {
	role := m.Role
	if role == "" {
		role = "user"
	}
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return []any{map[string]any{"role": role, "content": ""}}, nil
	}

	var plain string
	if json.Unmarshal(m.Content, &plain) == nil {
		return []any{map[string]any{"role": role, "content": plain}}, nil
	}

	var blocks []anthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, err
	}
	if role == "assistant" {
		return []any{anthropicAssistantMessage(blocks)}, nil
	}
	return anthropicUserMessages(blocks), nil
}

// anthropicAssistantMessage 保留 text/thinking/tool_use 的顺序语义：
// 同一 assistant 消息里的并行 tool_use 必须落在同一个 tool_calls 数组里。
func anthropicAssistantMessage(blocks []anthropicContentBlock) map[string]any {
	texts := []string{}
	reasoning := []string{}
	calls := []any{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "thinking":
			t := b.Thinking
			if t == "" {
				t = b.Text
			}
			if t != "" {
				reasoning = append(reasoning, t)
			}
		case "tool_use":
			calls = append(calls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": anthropicToolArguments(b.Input),
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	if len(reasoning) > 0 {
		msg["reasoning_content"] = strings.Join(reasoning, "\n")
	}
	return msg
}

// anthropicUserMessages 收集 user 的 text/image，遇到 tool_result 时先落盘前序内容，
// 再输出 tool 消息；tool_result 内图片提升为紧随其后的 user 多模态消息。
func anthropicUserMessages(blocks []anthropicContentBlock) []any {
	out := []any{}
	parts := []any{}
	hasImage := false

	flush := func() {
		if len(parts) == 0 {
			return
		}
		if hasImage {
			out = append(out, map[string]any{"role": "user", "content": parts})
		} else {
			texts := make([]string, 0, len(parts))
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if t, _ := pm["text"].(string); t != "" {
						texts = append(texts, t)
					}
				}
			}
			out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
		}
		parts = nil
		hasImage = false
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case "image":
			if u := anthropicImageURL(b.Source); u != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				hasImage = true
			}
		case "tool_result":
			flush()
			text, images := anthropicToolResultContent(b.Content)
			out = append(out, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text})
			if len(images) > 0 {
				out = append(out, map[string]any{"role": "user", "content": images})
			}
		}
	}
	flush()
	return out
}

// anthropicToolResultContent 解析 tool_result 的 content（字符串或 block 数组），
// 返回文本与需要提升的图片 parts。
func anthropicToolResultContent(raw json.RawMessage) (string, []any) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil
	}
	texts := []string{}
	images := []any{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "image":
			if u := anthropicImageURL(b.Source); u != "" {
				images = append(images, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		}
	}
	return strings.Join(texts, "\n"), images
}

func anthropicImageURL(src *anthropicImageSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return ""
		}
		return "data:" + src.MediaType + ";base64," + src.Data
	case "url":
		return src.URL
	default:
		return src.URL
	}
}

func anthropicToolArguments(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return "{}"
		}
		return s
	}
	return string(raw)
}

func anthropicToolsToChat(in []anthropicTool, meta *anthropicMeta) []any {
	out := make([]any, 0, len(in))
	for _, t := range in {
		if t.Name == "" {
			continue
		}
		meta.ToolKinds[t.Name] = "function"
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.InputSchema,
			},
		})
	}
	return out
}

func anthropicToolChoice(raw json.RawMessage, meta *anthropicMeta) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		default:
			return nil
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	switch obj.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if obj.Name == "" {
			return nil
		}
	default:
		return nil
	}
	if meta != nil {
		meta.ToolKinds[obj.Name] = "function"
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
}
