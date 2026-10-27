package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type anthropicSSEEvent struct {
	event string
	data  map[string]any
}

// parseAnthropicSSE splits an Anthropic SSE body into named events.
func parseAnthropicSSE(t *testing.T, body string) []anthropicSSEEvent {
	t.Helper()
	var events []anthropicSSEEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		ev := anthropicSSEEvent{}
		dataLine := ""
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		if dataLine == "" {
			t.Fatalf("SSE block without data line: %q", block)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(dataLine), &data); err != nil {
			t.Fatalf("invalid SSE data %q: %v", dataLine, err)
		}
		events = append(events, anthropicSSEEvent{event: ev.event, data: data})
	}
	return events
}

func anthropicEventTypes(events []anthropicSSEEvent) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.event)
	}
	return types
}

func anthropicEventsOf(events []anthropicSSEEvent, typ string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev.event == typ {
			out = append(out, ev.data)
		}
	}
	return out
}

func anthropicCountEvent(events []anthropicSSEEvent, typ string) int {
	return len(anthropicEventsOf(events, typ))
}

func assertAnthropicSequence(t *testing.T, got []anthropicSSEEvent, want ...string) {
	t.Helper()
	types := anthropicEventTypes(got)
	if len(types) != len(want) {
		t.Fatalf("event sequence = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event sequence = %v, want %v", types, want)
		}
	}
}

func runAnthropicStream(t *testing.T, input string, meta *anthropicMeta) ([]anthropicSSEEvent, string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	err := streamAnthropic(rec, io.NopCloser(strings.NewReader(input)), meta)
	return parseAnthropicSSE(t, rec.Body.String()), rec.Body.String(), err
}

func TestAnthropicStreamEventSequence(t *testing.T) {
	input := `data: {"id":"x","choices":[{"delta":{"content":"he"}}]}` + "\n\n" +
		`data: {"id":"x","choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}],"usage":{"completion_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "claude-sonnet"})
	if err != nil {
		t.Fatalf("streamAnthropic() error = %v\n%s", err, body)
	}
	assertAnthropicSequence(t, events,
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	)

	start := anthropicEventsOf(events, "message_start")[0]["message"].(map[string]any)
	if start["model"] != "claude-sonnet" {
		t.Fatalf("message_start model = %v", start["model"])
	}
	if start["id"] == "" || !strings.HasPrefix(start["id"].(string), "msg_") {
		t.Fatalf("message_start id = %v", start["id"])
	}
	if content, ok := start["content"].([]any); !ok || len(content) != 0 {
		t.Fatalf("message_start content = %v", start["content"])
	}

	blockStart := anthropicEventsOf(events, "content_block_start")[0]
	if blockStart["index"] != float64(0) {
		t.Fatalf("content_block_start index = %v", blockStart["index"])
	}
	cb := blockStart["content_block"].(map[string]any)
	if cb["type"] != "text" || cb["text"] != "" {
		t.Fatalf("content_block = %v", cb)
	}

	var text strings.Builder
	for _, d := range anthropicEventsOf(events, "content_block_delta") {
		delta := d["delta"].(map[string]any)
		if delta["type"] != "text_delta" {
			t.Fatalf("delta type = %v", delta["type"])
		}
		text.WriteString(delta["text"].(string))
	}
	if text.String() != "hello" {
		t.Fatalf("text deltas = %q, want %q", text.String(), "hello")
	}

	stop := anthropicEventsOf(events, "content_block_stop")[0]
	if stop["index"] != float64(0) {
		t.Fatalf("content_block_stop index = %v", stop["index"])
	}

	md := anthropicEventsOf(events, "message_delta")[0]
	if md["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", md["delta"])
	}
	if md["usage"].(map[string]any)["output_tokens"] != float64(7) {
		t.Fatalf("output_tokens = %v", md["usage"])
	}
	if n := anthropicCountEvent(events, "message_stop"); n != 1 {
		t.Fatalf("message_stop count = %d, want 1", n)
	}
}

func TestAnthropicStreamThinkingThenText(t *testing.T) {
	input := `data: {"choices":[{"delta":{"reasoning_content":"think"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "m"})
	if err != nil {
		t.Fatalf("streamAnthropic() error = %v\n%s", err, body)
	}
	assertAnthropicSequence(t, events,
		"message_start",
		"content_block_start", "content_block_delta",
		"content_block_start", "content_block_delta",
		"content_block_stop", "content_block_stop",
		"message_delta", "message_stop",
	)

	starts := anthropicEventsOf(events, "content_block_start")
	if starts[0]["index"] != float64(0) || starts[0]["content_block"].(map[string]any)["type"] != "thinking" {
		t.Fatalf("thinking block start = %v", starts[0])
	}
	if starts[1]["index"] != float64(1) || starts[1]["content_block"].(map[string]any)["type"] != "text" {
		t.Fatalf("text block start = %v", starts[1])
	}

	deltas := anthropicEventsOf(events, "content_block_delta")
	if deltas[0]["delta"].(map[string]any)["type"] != "thinking_delta" ||
		deltas[0]["delta"].(map[string]any)["thinking"] != "think" {
		t.Fatalf("thinking delta = %v", deltas[0])
	}
	if deltas[1]["delta"].(map[string]any)["type"] != "text_delta" ||
		deltas[1]["delta"].(map[string]any)["text"] != "answer" {
		t.Fatalf("text delta = %v", deltas[1])
	}

	stops := anthropicEventsOf(events, "content_block_stop")
	if stops[0]["index"] != float64(0) || stops[1]["index"] != float64(1) {
		t.Fatalf("content_block_stop indexes = %v", stops)
	}
}

func TestAnthropicStreamToolUseAccumulatesArguments(t *testing.T) {
	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"toolu_1","function":{"name":"","arguments":"{\"city\""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"get_weather","arguments":":\"SF\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "m"})
	if err != nil {
		t.Fatalf("streamAnthropic() error = %v\n%s", err, body)
	}
	assertAnthropicSequence(t, events,
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	)

	start := anthropicEventsOf(events, "content_block_start")[0]
	cb := start["content_block"].(map[string]any)
	if cb["type"] != "tool_use" {
		t.Fatalf("content_block type = %v", cb["type"])
	}
	if cb["id"] != "toolu_1" {
		t.Fatalf("tool id = %v", cb["id"])
	}
	if cb["name"] != "get_weather" {
		t.Fatalf("tool name = %v", cb["name"])
	}
	if input, ok := cb["input"].(map[string]any); !ok || len(input) != 0 {
		t.Fatalf("tool input = %v", cb["input"])
	}

	var partial strings.Builder
	for _, d := range anthropicEventsOf(events, "content_block_delta") {
		delta := d["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			t.Fatalf("delta type = %v", delta["type"])
		}
		partial.WriteString(delta["partial_json"].(string))
	}
	if partial.String() != `{"city":"SF"}` {
		t.Fatalf("partial_json = %q", partial.String())
	}

	md := anthropicEventsOf(events, "message_delta")[0]
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", md["delta"])
	}
}

func TestAnthropicStreamToolUseSplitNameAtFinish(t *testing.T) {
	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"toolu_2","function":{"name":"get_","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "m"})
	if err != nil {
		t.Fatalf("streamAnthropic() error = %v\n%s", err, body)
	}
	assertAnthropicSequence(t, events,
		"message_start",
		"content_block_start", "content_block_stop",
		"message_delta", "message_stop",
	)

	start := anthropicEventsOf(events, "content_block_start")[0]
	cb := start["content_block"].(map[string]any)
	if cb["name"] != "get_weather" {
		t.Fatalf("accumulated tool name = %v", cb["name"])
	}
	if n := anthropicCountEvent(events, "content_block_delta"); n != 0 {
		t.Fatalf("content_block_delta count = %d, want 0", n)
	}
	md := anthropicEventsOf(events, "message_delta")[0]
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", md["delta"])
	}
}

func TestAnthropicStreamToolUseSplitNameWithArguments(t *testing.T) {
	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"toolu_3","function":{"name":"get_","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"{\"city\""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"SF\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "m"})
	if err != nil {
		t.Fatalf("streamAnthropic() error = %v\n%s", err, body)
	}
	assertAnthropicSequence(t, events,
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	)

	start := anthropicEventsOf(events, "content_block_start")[0]
	cb := start["content_block"].(map[string]any)
	if cb["name"] != "get_weather" {
		t.Fatalf("content_block_start name = %v, want get_weather", cb["name"])
	}

	var partial strings.Builder
	for _, d := range anthropicEventsOf(events, "content_block_delta") {
		delta := d["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			t.Fatalf("delta type = %v", delta["type"])
		}
		partial.WriteString(delta["partial_json"].(string))
	}
	if partial.String() != `{"city":"SF"}` {
		t.Fatalf("partial_json = %q", partial.String())
	}
	if !json.Valid([]byte(partial.String())) {
		t.Fatalf("partial_json is not valid JSON: %q", partial.String())
	}
}

func TestAnthropicStreamErrorFrame(t *testing.T) {
	input := `data: {"error":{"message":"boom","type":"upstream_error"}}` + "\n\n" +
		"data: [DONE]\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "m"})
	if !errors.Is(err, errAnthropicStreamFailed) {
		t.Fatalf("error = %v, want errAnthropicStreamFailed\n%s", err, body)
	}
	assertAnthropicSequence(t, events, "message_start", "error")
	if n := anthropicCountEvent(events, "message_stop"); n != 0 {
		t.Fatalf("message_stop count = %d, want 0", n)
	}
	ev := anthropicEventsOf(events, "error")[0]
	if ev["type"] != "error" {
		t.Fatalf("error event type = %v", ev["type"])
	}
	inner := ev["error"].(map[string]any)
	if inner["type"] != "api_error" {
		t.Fatalf("error.type = %v", inner["type"])
	}
	if inner["message"] != "boom" {
		t.Fatalf("error.message = %v", inner["message"])
	}
}

func TestAnthropicStreamEmpty(t *testing.T) {
	events, body, err := runAnthropicStream(t, "", &anthropicMeta{Model: "m"})
	if !errors.Is(err, errAnthropicStreamFailed) {
		t.Fatalf("error = %v, want errAnthropicStreamFailed\n%s", err, body)
	}
	assertAnthropicSequence(t, events, "message_start", "error")
	if n := anthropicCountEvent(events, "message_stop"); n != 0 {
		t.Fatalf("message_stop count = %d, want 0", n)
	}
}

func TestAnthropicStreamDoneWithoutFinishReason(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "m"})
	if err != nil {
		t.Fatalf("streamAnthropic() error = %v\n%s", err, body)
	}
	if n := anthropicCountEvent(events, "message_stop"); n != 1 {
		t.Fatalf("message_stop count = %d, want 1", n)
	}
	md := anthropicEventsOf(events, "message_delta")[0]
	if md["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", md["delta"])
	}
}

func TestAnthropicStreamFinishWithoutDone(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"

	events, body, err := runAnthropicStream(t, input, &anthropicMeta{Model: "m"})
	if !errors.Is(err, errAnthropicStreamFailed) {
		t.Fatalf("error = %v, want errAnthropicStreamFailed\n%s", err, body)
	}
	if n := anthropicCountEvent(events, "message_stop"); n != 0 {
		t.Fatalf("message_stop count = %d, want 0", n)
	}
	if n := anthropicCountEvent(events, "error"); n != 1 {
		t.Fatalf("error event count = %d, want 1", n)
	}
}
