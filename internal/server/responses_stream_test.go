package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

type responsesSSEEvent struct {
	event string
	data  map[string]any
}

// parseResponsesSSE splits a Responses SSE body into events and counts the
// terminal [DONE] frames.
func parseResponsesSSE(t *testing.T, body string) ([]responsesSSEEvent, int) {
	t.Helper()
	var events []responsesSSEEvent
	done := 0
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		ev := responsesSSEEvent{}
		dataLine := ""
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		if dataLine == "[DONE]" {
			done++
			events = append(events, responsesSSEEvent{event: "DONE"})
			continue
		}
		if dataLine == "" {
			t.Fatalf("SSE block without data line: %q", block)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(dataLine), &data); err != nil {
			t.Fatalf("invalid SSE data %q: %v", dataLine, err)
		}
		events = append(events, responsesSSEEvent{event: ev.event, data: data})
	}
	return events, done
}

func responsesEventTypes(events []responsesSSEEvent) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.event)
	}
	return types
}

func responsesFindEvent(t *testing.T, events []responsesSSEEvent, typ string) map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev.event == typ {
			return ev.data
		}
	}
	t.Fatalf("event %q not found in %v", typ, responsesEventTypes(events))
	return nil
}

func responsesCountEvent(events []responsesSSEEvent, typ string) int {
	n := 0
	for _, ev := range events {
		if ev.event == typ {
			n++
		}
	}
	return n
}

func responsesCompletedOutput(t *testing.T, events []responsesSSEEvent) []any {
	t.Helper()
	completed := responsesFindEvent(t, events, "response.completed")["response"].(map[string]any)
	output, ok := completed["output"].([]any)
	if !ok {
		t.Fatalf("completed output is not an array: %v", completed["output"])
	}
	return output
}

func assertResponsesSequence(t *testing.T, events []responsesSSEEvent) {
	t.Helper()
	last := -1
	for _, ev := range events {
		if ev.event == "DONE" {
			continue
		}
		raw, ok := ev.data["sequence_number"]
		if !ok {
			t.Fatalf("%s event missing sequence_number: %v", ev.event, ev.data)
		}
		n, ok := raw.(float64)
		if !ok {
			t.Fatalf("%s sequence_number is not a number: %v", ev.event, raw)
		}
		if int(n) <= last {
			t.Fatalf("sequence_number not monotonically increasing: %v after %d", n, last)
		}
		last = int(n)
		if typ, _ := ev.data["type"].(string); typ != ev.event {
			t.Fatalf("event line %q does not match data.type %q", ev.event, typ)
		}
	}
}

func assertResponsesOrder(t *testing.T, events []responsesSSEEvent, want []string) {
	t.Helper()
	got := responsesEventTypes(events)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("event order mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func TestResponsesStreamEventSequence(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
			"data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]," +
			"\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] count=%d want 1", done)
	}
	assertResponsesOrder(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
		"DONE",
	})
	assertResponsesSequence(t, events)
	if n := responsesCountEvent(events, "response.completed"); n != 1 {
		t.Fatalf("response.completed count=%d want 1", n)
	}
	if n := responsesCountEvent(events, "response.failed"); n != 0 {
		t.Fatalf("response.failed count=%d want 0", n)
	}

	created := responsesFindEvent(t, events, "response.created")["response"].(map[string]any)
	completed := responsesFindEvent(t, events, "response.completed")["response"].(map[string]any)
	if created["id"] != completed["id"] || created["model"] != completed["model"] {
		t.Fatalf("created/completed mismatch: %v vs %v", created, completed)
	}
	if completed["status"] != "completed" {
		t.Fatalf("completed status=%v", completed["status"])
	}
	textDone := responsesFindEvent(t, events, "response.output_text.done")
	if textDone["text"] != "hello" {
		t.Fatalf("output_text.done text=%v want hello", textDone["text"])
	}
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 3 || usage["output_tokens"].(float64) != 2 || usage["total_tokens"].(float64) != 5 {
		t.Fatalf("usage=%v", usage)
	}
	output := responsesCompletedOutput(t, events)
	if len(output) != 1 {
		t.Fatalf("output items=%d want 1", len(output))
	}
}

func TestResponsesStreamReasoning(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"th\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"ink\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] count=%d want 1", done)
	}
	assertResponsesOrder(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.completed",
		"DONE",
	})
	assertResponsesSequence(t, events)
	textDone := responsesFindEvent(t, events, "response.reasoning_summary_text.done")
	if textDone["text"] != "think" {
		t.Fatalf("reasoning text=%v want think", textDone["text"])
	}
	item := responsesFindEvent(t, events, "response.output_item.done")["item"].(map[string]any)
	if item["type"] != "reasoning" {
		t.Fatalf("done item type=%v want reasoning", item["type"])
	}
	summary := item["summary"].([]any)[0].(map[string]any)
	if summary["text"] != "think" {
		t.Fatalf("summary text=%v want think", summary["text"])
	}
	completed := responsesFindEvent(t, events, "response.completed")["response"].(map[string]any)
	if output := completed["output"].([]any); len(output) != 1 || output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("completed output=%v", output)
	}
}

func TestResponsesStreamToolCalls(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"get_\",\"arguments\":\"{\\\"ci\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"weather\",\"arguments\":\"ty\\\":\\\"SF\\\"}\"}},{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"look\",\"arguments\":\"{\\\"q\\\":\\\"x\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] count=%d want 1", done)
	}
	assertResponsesSequence(t, events)
	if n := responsesCountEvent(events, "response.output_item.added"); n != 2 {
		t.Fatalf("output_item.added count=%d want 2", n)
	}
	if n := responsesCountEvent(events, "response.function_call_arguments.done"); n != 2 {
		t.Fatalf("arguments.done count=%d want 2", n)
	}
	if n := responsesCountEvent(events, "response.output_item.done"); n != 2 {
		t.Fatalf("output_item.done count=%d want 2", n)
	}
	if n := responsesCountEvent(events, "response.completed"); n != 1 {
		t.Fatalf("response.completed count=%d want 1", n)
	}

	output := responsesCompletedOutput(t, events)
	if len(output) != 2 {
		t.Fatalf("output items=%d want 2", len(output))
	}
	first := output[0].(map[string]any)
	second := output[1].(map[string]any)
	if first["type"] != "function_call" || first["call_id"] != "call_a" || first["name"] != "get_weather" {
		t.Fatalf("first function_call=%v", first)
	}
	if first["arguments"] != `{"city":"SF"}` {
		t.Fatalf("first arguments=%v", first["arguments"])
	}
	if second["call_id"] != "call_b" || second["name"] != "look" || second["arguments"] != `{"q":"x"}` {
		t.Fatalf("second function_call=%v", second)
	}

	doneEvents := []map[string]any{}
	for _, ev := range events {
		if ev.event == "response.output_item.done" {
			doneEvents = append(doneEvents, ev.data["item"].(map[string]any))
		}
	}
	if doneEvents[0]["name"] != "get_weather" || doneEvents[1]["name"] != "look" {
		t.Fatalf("done item names=%v/%v", doneEvents[0]["name"], doneEvents[1]["name"])
	}
}

func TestResponsesStreamToolCallRepeatedName(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"a\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"b\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"c\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, _ := parseResponsesSSE(t, rec.Body.String())
	output := responsesCompletedOutput(t, events)
	if len(output) != 1 {
		t.Fatalf("output items=%d want 1", len(output))
	}
	call := output[0].(map[string]any)
	if call["name"] != "Bash" {
		t.Fatalf("name=%v want Bash (must not duplicate repeated full name)", call["name"])
	}
	if call["arguments"] != "abc" {
		t.Fatalf("arguments=%v want abc", call["arguments"])
	}
	if n := responsesCountEvent(events, "response.output_item.added"); n != 1 {
		t.Fatalf("output_item.added count=%d want 1", n)
	}
}

func TestResponsesStreamToolCallDelayedAdded(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_late\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"1}\"}}]},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, _ := parseResponsesSSE(t, rec.Body.String())
	assertResponsesOrder(t, events, []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
		"DONE",
	})
	added := responsesFindEvent(t, events, "response.output_item.added")["item"].(map[string]any)
	if added["name"] != "lookup" || added["call_id"] != "call_late" {
		t.Fatalf("delayed added item=%v want full name/call_id", added)
	}
	output := responsesCompletedOutput(t, events)
	call := output[0].(map[string]any)
	if call["arguments"] != `{"a":1}` {
		t.Fatalf("arguments=%v want {\"a\":1}", call["arguments"])
	}
}

func TestResponsesStreamToolCallLateID(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_late\",\"function\":{\"name\":\"lookup\",\"arguments\":\"1}\"}}]},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, _ := parseResponsesSSE(t, rec.Body.String())
	if n := responsesCountEvent(events, "response.output_item.added"); n != 1 {
		t.Fatalf("output_item.added count=%d want 1 (late id must attach, not fork)", n)
	}
	added := responsesFindEvent(t, events, "response.output_item.added")["item"].(map[string]any)
	if added["call_id"] != "call_late" || added["name"] != "lookup" {
		t.Fatalf("added item=%v", added)
	}
	output := responsesCompletedOutput(t, events)
	if len(output) != 1 {
		t.Fatalf("output items=%d want 1", len(output))
	}
	call := output[0].(map[string]any)
	if call["call_id"] != "call_late" || call["name"] != "lookup" || call["arguments"] != `{"a":1}` {
		t.Fatalf("function_call=%v", call)
	}
}

func TestResponsesStreamToolCallNoIndexParallel(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"alpha\",\"arguments\":\"a1\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"beta\",\"arguments\":\"b1\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"b2\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, _ := parseResponsesSSE(t, rec.Body.String())
	output := responsesCompletedOutput(t, events)
	if len(output) != 2 {
		t.Fatalf("output items=%d want 2", len(output))
	}
	first := output[0].(map[string]any)
	second := output[1].(map[string]any)
	if first["call_id"] != "call_a" || first["name"] != "alpha" || first["arguments"] != "a1" {
		t.Fatalf("first function_call=%v", first)
	}
	if second["call_id"] != "call_b" || second["name"] != "beta" || second["arguments"] != "b1b2" {
		t.Fatalf("second function_call=%v", second)
	}
}

func TestResponsesStreamUpstreamErrorFrame(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_error\",\"code\":\"6004\"}}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"})
	if !errors.Is(err, errResponsesStreamFailed) {
		t.Fatalf("err=%v want errResponsesStreamFailed", err)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] count=%d want 1", done)
	}
	assertResponsesSequence(t, events)
	if n := responsesCountEvent(events, "response.failed"); n != 1 {
		t.Fatalf("response.failed count=%d want 1", n)
	}
	if n := responsesCountEvent(events, "response.completed"); n != 0 {
		t.Fatalf("response.completed count=%d want 0", n)
	}
	failed := responsesFindEvent(t, events, "response.failed")["response"].(map[string]any)
	errObj := failed["error"].(map[string]any)
	if errObj["message"] != "rate limited" || errObj["type"] != "rate_limit_error" || errObj["code"] != "6004" {
		t.Fatalf("error=%v", errObj)
	}
}

func TestResponsesStreamErrorFrameNestedData(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"error\":{\"data\":{\"code\":11140,\"msg\":\"blocked\"}}}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"})
	if !errors.Is(err, errResponsesStreamFailed) {
		t.Fatalf("err=%v want errResponsesStreamFailed", err)
	}
	events, _ := parseResponsesSSE(t, rec.Body.String())
	failed := responsesFindEvent(t, events, "response.failed")["response"].(map[string]any)
	errObj := failed["error"].(map[string]any)
	if errObj["message"] != "blocked" {
		t.Fatalf("message=%v want blocked", errObj["message"])
	}
	if errObj["code"] != "11140" {
		t.Fatalf("code=%v want \"11140\" (numeric code must be stringified)", errObj["code"])
	}
	if errObj["type"] != "upstream_error" {
		t.Fatalf("type=%v want upstream_error", errObj["type"])
	}
}

func TestResponsesStreamErrorExtraction(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		message string
		errType string
		code    string
	}{
		{"nested-message", map[string]any{"message": map[string]any{"message": "deep"}}, "deep", "upstream_error", "upstream_error"},
		{"numeric-code", map[string]any{"msg": "bad", "code": float64(11102)}, "bad", "upstream_error", "11102"},
		{"data-object", map[string]any{"data": map[string]any{"code": float64(7), "msg": "denied"}}, "denied", "upstream_error", "7"},
		{"string", "plain failure", "plain failure", "upstream_error", "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message, errType, code := responsesStreamError(tc.in)
			if message != tc.message || errType != tc.errType || code != tc.code {
				t.Fatalf("got (%q,%q,%q) want (%q,%q,%q)", message, errType, code, tc.message, tc.errType, tc.code)
			}
		})
	}
}

func TestResponsesStreamEmpty(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"comments-only", ": keepalive\n\n: still here\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := streamResponses(rec, io.NopCloser(strings.NewReader(tc.body)), &responsesMeta{Model: "glm-5.2"})
			if !errors.Is(err, errResponsesStreamFailed) {
				t.Fatalf("err=%v want errResponsesStreamFailed", err)
			}
			events, done := parseResponsesSSE(t, rec.Body.String())
			if done != 1 {
				t.Fatalf("[DONE] count=%d want 1", done)
			}
			assertResponsesSequence(t, events)
			if n := responsesCountEvent(events, "response.failed"); n != 1 {
				t.Fatalf("response.failed count=%d want 1", n)
			}
			if n := responsesCountEvent(events, "response.completed"); n != 0 {
				t.Fatalf("response.completed count=%d want 0", n)
			}
			failed := responsesFindEvent(t, events, "response.failed")["response"].(map[string]any)
			errObj := failed["error"].(map[string]any)
			if errObj["code"] != "upstream_parse" {
				t.Fatalf("error code=%v want upstream_parse", errObj["code"])
			}
		})
	}
}

func TestResponsesStreamDoneOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, io.NopCloser(strings.NewReader("data: [DONE]\n\n")), &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatalf("done-only stream should succeed: %v", err)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] count=%d want 1", done)
	}
	if n := responsesCountEvent(events, "response.completed"); n != 1 {
		t.Fatalf("response.completed count=%d want 1", n)
	}
	if n := responsesCountEvent(events, "response.failed"); n != 0 {
		t.Fatalf("response.failed count=%d want 0", n)
	}
	if output := responsesCompletedOutput(t, events); len(output) != 0 {
		t.Fatalf("output items=%d want 0", len(output))
	}
}

func TestResponsesStreamDoneWithoutFinishReason(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatalf("sawDone should be sufficient for success: %v", err)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] count=%d want 1", done)
	}
	if n := responsesCountEvent(events, "response.completed"); n != 1 {
		t.Fatalf("response.completed count=%d want 1", n)
	}
	if n := responsesCountEvent(events, "response.failed"); n != 0 {
		t.Fatalf("response.failed count=%d want 0", n)
	}
}

func TestResponsesStreamTruncated(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"no-finish-no-done",
			"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		},
		{
			"finish-without-done",
			"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}]}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := streamResponses(rec, io.NopCloser(strings.NewReader(tc.body)), &responsesMeta{Model: "glm-5.2"})
			if !errors.Is(err, errResponsesStreamFailed) {
				t.Fatalf("err=%v want errResponsesStreamFailed", err)
			}
			events, done := parseResponsesSSE(t, rec.Body.String())
			if done != 1 {
				t.Fatalf("[DONE] count=%d want 1", done)
			}
			assertResponsesSequence(t, events)
			if n := responsesCountEvent(events, "response.failed"); n != 1 {
				t.Fatalf("response.failed count=%d want 1", n)
			}
			if n := responsesCountEvent(events, "response.completed"); n != 0 {
				t.Fatalf("response.completed count=%d want 0", n)
			}
			failed := responsesFindEvent(t, events, "response.failed")["response"].(map[string]any)
			errObj := failed["error"].(map[string]any)
			if errObj["code"] != "upstream_truncated" {
				t.Fatalf("error code=%v want upstream_truncated", errObj["code"])
			}
		})
	}
}

func TestResponsesStreamNonDeltaMessage(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"reasoning_content\":\"why\",\"content\":\"answer\"," +
			"\"tool_calls\":[{\"id\":\"call_x\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}," +
			"\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if err := streamResponses(rec, rc, &responsesMeta{Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	events, done := parseResponsesSSE(t, rec.Body.String())
	if done != 1 {
		t.Fatalf("[DONE] count=%d want 1", done)
	}
	if n := responsesCountEvent(events, "response.completed"); n != 1 {
		t.Fatalf("response.completed count=%d want 1", n)
	}
	output := responsesCompletedOutput(t, events)
	if len(output) != 3 {
		t.Fatalf("output items=%d want reasoning+message+function_call", len(output))
	}
	reasoning := output[0].(map[string]any)
	message := output[1].(map[string]any)
	call := output[2].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["summary"].([]any)[0].(map[string]any)["text"] != "why" {
		t.Fatalf("reasoning item=%v", reasoning)
	}
	if message["type"] != "message" || message["content"].([]any)[0].(map[string]any)["text"] != "answer" {
		t.Fatalf("message item=%v", message)
	}
	if call["type"] != "function_call" || call["call_id"] != "call_x" || call["name"] != "lookup" {
		t.Fatalf("function_call item=%v", call)
	}
	if call["arguments"] != `{"q":"x"}` {
		t.Fatalf("function_call arguments=%v", call["arguments"])
	}
}

func TestScanSSEParsesDataFrames(t *testing.T) {
	input := ": comment\n\n" +
		"event: message\n" +
		"data: {\"a\":1}\n\n" +
		"data:{\"b\":2}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"after\":\"done\"}\n\n"
	var got []string
	if err := upstream.ScanSSE(strings.NewReader(input), func(payload string) error {
		got = append(got, payload)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("payloads=%v want [{\"a\":1} {\"b\":2}]", got)
	}
}

func TestScanSSESkipsCommentsAndBlankLines(t *testing.T) {
	input := "\n: ping\n\n   \ndata: one\n\n: trailing\ndata: two\n\n"
	var got []string
	if err := upstream.ScanSSE(strings.NewReader(input), func(payload string) error {
		got = append(got, payload)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "one,two" {
		t.Fatalf("payloads=%v", got)
	}
}

func TestScanSSELargeLine(t *testing.T) {
	big := strings.Repeat("x", 100*1024)
	input := "data: " + big + "\n\n"
	var got string
	if err := upstream.ScanSSE(strings.NewReader(input), func(payload string) error {
		got = payload
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != big {
		t.Fatalf("payload length=%d want %d", len(got), len(big))
	}
}

func TestScanSSEPropagatesCallbackError(t *testing.T) {
	sentinel := errors.New("stop now")
	err := upstream.ScanSSE(strings.NewReader("data: one\n\ndata: two\n\n"), func(string) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v want sentinel", err)
	}
}

func TestScanSSEWithDoneReportsDone(t *testing.T) {
	var got []string
	sawDone, err := upstream.ScanSSEWithDone(
		strings.NewReader("data: one\n\ndata: [DONE]\n\ndata: two\n\n"),
		func(payload string) error {
			got = append(got, payload)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !sawDone {
		t.Fatal("sawDone=false want true")
	}
	if strings.Join(got, ",") != "one" {
		t.Fatalf("payloads=%v want [one]", got)
	}

	sawDone, err = upstream.ScanSSEWithDone(strings.NewReader("data: one\n\n"), func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sawDone {
		t.Fatal("sawDone=true want false for EOF without [DONE]")
	}

	sentinel := errors.New("boom")
	sawDone, err = upstream.ScanSSEWithDone(strings.NewReader("data: one\n\n"), func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v want sentinel", err)
	}
	if sawDone {
		t.Fatal("sawDone=true want false when callback aborts")
	}
}
