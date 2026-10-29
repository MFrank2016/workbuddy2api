package server

import "encoding/json"

// anthropicRequest Anthropic Messages 请求中本网关关心的字段。
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      json.RawMessage    `json:"system"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools"`
	ToolChoice  json.RawMessage    `json:"tool_choice"`
	Temperature *float64           `json:"temperature"`
	TopP        *float64           `json:"top_p"`
	Stop        []string           `json:"stop_sequences"`
	Stream      bool               `json:"stream"`
	Metadata    map[string]any     `json:"metadata"`
}

// anthropicMessage content 既可能是字符串，也可能是 block 数组。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// anthropicMeta 翻译过程中需要回传给响应/流式层的元信息。
type anthropicMeta struct {
	Model     string
	Stream    bool
	ToolKinds map[string]string
}

// anthropicContentBlock 覆盖 text / thinking / image / tool_use / tool_result。
type anthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text"`
	Thinking  string                `json:"thinking"`
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Input     json.RawMessage       `json:"input"`
	ToolUseID string                `json:"tool_use_id"`
	Content   json.RawMessage       `json:"content"`
	Source    *anthropicImageSource `json:"source"`
}
