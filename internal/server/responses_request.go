package server

import (
	"encoding/json"
)

type responsesMeta struct {
	Model        string
	Stream       bool
	Instructions string
	ToolKinds    map[string]string
}

func buildChatBodyFromResponses(raw []byte) ([]byte, *responsesMeta, error) {
	var in struct {
		Model        string          `json:"model"`
		Stream       bool            `json:"stream"`
		Instructions string          `json:"instructions"`
		Input        json.RawMessage `json:"input"`
		Tools        []responsesTool `json:"tools"`
		MaxOutput    *int            `json:"max_output_tokens"`
		Reasoning    *struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		} `json:"reasoning"`
		Parallel *bool `json:"parallel_tool_calls"`
		Text     *struct {
			Verbosity string `json:"verbosity"`
		} `json:"text"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, nil, err
	}

	meta := &responsesMeta{
		Model:        in.Model,
		Stream:       in.Stream,
		Instructions: in.Instructions,
		ToolKinds:    map[string]string{},
	}

	msgs := []any{}
	if in.Instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": in.Instructions})
	}
	if len(in.Input) > 0 && string(in.Input) != "null" {
		items, err := parseResponsesInput(in.Input)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, items...)
	}

	body := map[string]any{"model": in.Model, "messages": msgs, "stream": true}
	if in.MaxOutput != nil {
		body["max_tokens"] = *in.MaxOutput
	}
	if in.Reasoning != nil {
		if in.Reasoning.Effort != "" {
			body["reasoning_effort"] = in.Reasoning.Effort
		}
		if in.Reasoning.Summary != "" {
			body["reasoning_summary"] = in.Reasoning.Summary
		}
	}
	if in.Text != nil && in.Text.Verbosity != "" {
		body["verbosity"] = in.Text.Verbosity
	}
	if len(in.Tools) > 0 {
		body["tools"] = translateResponsesTools(in.Tools, meta)
	}
	if in.Parallel != nil {
		body["parallel_tool_calls"] = *in.Parallel
	}
	if choice := translateResponsesToolChoice(in.ToolChoice); choice != nil {
		body["tool_choice"] = choice
	}

	out, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return out, meta, nil
}

func translateResponsesTools(in []responsesTool, meta *responsesMeta) []any {
	out := make([]any, 0, len(in))
	for _, t := range in {
		switch t.Type {
		case "function":
			meta.ToolKinds[t.Name] = "function"
			out = append(out, map[string]any{"type": "function", "function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			}})
		}
	}
	return out
}

func translateResponsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch name {
		case "auto", "none", "required":
			return name
		default:
			return nil
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if obj.Type != "function" {
		return nil
	}
	if obj.Name == "" && obj.Function != nil {
		obj.Name = obj.Function.Name
	}
	if obj.Name == "" {
		return nil
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
}
