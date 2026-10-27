package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicURLImageToChat(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
	  {"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}
	]}]}`)
	body, _, err := buildChatBodyFromAnthropic(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	parts := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts=%v", parts)
	}
	img := parts[0].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("img=%v", img)
	}
	if url := img["image_url"].(map[string]any)["url"]; url != "https://example.com/a.png" {
		t.Fatalf("url=%v", url)
	}
}

func TestAnthropicToolResultImagePromoted(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
	    {"type":"text","text":"ok"},
	    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
	  ]}]}
	]}`)
	body, _, err := buildChatBodyFromAnthropic(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want assistant+tool+user image: %v", len(msgs), msgs)
	}
	asst := msgs[0].(map[string]any)
	if asst["tool_calls"] == nil {
		t.Fatalf("assistant tool_calls missing: %v", asst)
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" || tool["content"] != "ok" {
		t.Fatalf("tool msg=%v", tool)
	}
	user := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("image message role=%v", user["role"])
	}
	parts := user["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("image parts=%v", parts)
	}
	img := parts[0].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("img=%v", img)
	}
	url := img["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("url=%v", url)
	}
}
