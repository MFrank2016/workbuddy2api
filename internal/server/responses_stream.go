package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// errResponsesStreamFailed marks an in-band stream failure: response.failed and
// [DONE] were already written to the client, but the caller should still treat
// the request as failed (logging/account policy).
var errResponsesStreamFailed = errors.New("responses stream failed")

// responsesStreamItem tracks one output item while its deltas are replayed from
// the upstream chat stream. Items are ordered by first appearance so output
// indexes stay stable for the whole stream.
type responsesStreamItem struct {
	kind          string
	index         int
	upstreamIndex int
	id            string
	callID        string
	name          string
	text          strings.Builder
	args          strings.Builder
	pendingArgs   []string
	added         bool
	partAdded     bool
}

// streamResponses converts an upstream chat SSE stream into Responses API
// events. It always terminates with exactly one "data: [DONE]". Success ends
// with response.completed; any upstream error frame, empty stream, scan failure
// or truncation ends with response.failed and never response.completed.
func streamResponses(w http.ResponseWriter, rc io.ReadCloser, meta *responsesMeta) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	model := ""
	if meta != nil {
		model = meta.Model
	}
	respID := "resp_" + randomHex(12)
	createdAt := time.Now().Unix()

	seq := 0
	writeEvent := func(eventType string, fields map[string]any) error {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["type"] = eventType
		fields["sequence_number"] = seq
		seq++
		payload, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, "event: "+eventType+"\ndata: "+string(payload)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeDone := func() error {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	baseResponse := func(status string) map[string]any {
		return map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     status,
			"model":      model,
			"output":     []any{},
		}
	}

	items := []*responsesStreamItem{}
	newItem := func(kind string) *responsesStreamItem {
		it := &responsesStreamItem{kind: kind, index: len(items)}
		items = append(items, it)
		return it
	}

	var reasoningItem, messageItem *responsesStreamItem

	startReasoning := func() (*responsesStreamItem, error) {
		if reasoningItem == nil {
			reasoningItem = newItem("reasoning")
			reasoningItem.id = "rs_" + randomHex(8)
		}
		it := reasoningItem
		if !it.added {
			it.added = true
			if err := writeEvent("response.output_item.added", map[string]any{
				"output_index": it.index,
				"item": map[string]any{
					"id":      it.id,
					"type":    "reasoning",
					"summary": []any{},
				},
			}); err != nil {
				return nil, err
			}
		}
		if !it.partAdded {
			it.partAdded = true
			if err := writeEvent("response.reasoning_summary_part.added", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": ""},
			}); err != nil {
				return nil, err
			}
		}
		return it, nil
	}

	startMessage := func() (*responsesStreamItem, error) {
		if messageItem == nil {
			messageItem = newItem("message")
			messageItem.id = "msg_" + randomHex(8)
		}
		it := messageItem
		if !it.added {
			it.added = true
			if err := writeEvent("response.output_item.added", map[string]any{
				"output_index": it.index,
				"item": map[string]any{
					"id":      it.id,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				},
			}); err != nil {
				return nil, err
			}
		}
		if !it.partAdded {
			it.partAdded = true
			if err := writeEvent("response.content_part.added", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}); err != nil {
				return nil, err
			}
		}
		return it, nil
	}

	handleReasoning := func(text string) error {
		if text == "" {
			return nil
		}
		it, err := startReasoning()
		if err != nil {
			return err
		}
		it.text.WriteString(text)
		return writeEvent("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       it.id,
			"output_index":  it.index,
			"summary_index": 0,
			"delta":         text,
		})
	}
	handleContent := func(text string) error {
		if text == "" {
			return nil
		}
		it, err := startMessage()
		if err != nil {
			return err
		}
		it.text.WriteString(text)
		return writeEvent("response.output_text.delta", map[string]any{
			"item_id":       it.id,
			"output_index":  it.index,
			"content_index": 0,
			"delta":         text,
		})
	}

	toolByIndex := map[int]*responsesStreamItem{}
	toolIDIndex := map[string]int{}
	var lastToolItem *responsesStreamItem
	toolSeq := 0
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolByIndex[idx]; !used {
				return idx
			}
		}
	}

	// ensureToolAdded emits the delayed output_item.added for a function call and
	// flushes any argument deltas buffered while the item was not yet addressable.
	ensureToolAdded := func(it *responsesStreamItem) error {
		if it.added {
			return nil
		}
		it.added = true
		if err := writeEvent("response.output_item.added", map[string]any{
			"output_index": it.index,
			"item": map[string]any{
				"id":        it.id,
				"type":      "function_call",
				"call_id":   it.callID,
				"name":      it.name,
				"arguments": "",
				"status":    "in_progress",
			},
		}); err != nil {
			return err
		}
		for _, delta := range it.pendingArgs {
			if err := writeEvent("response.function_call_arguments.delta", map[string]any{
				"item_id":      it.id,
				"output_index": it.index,
				"delta":        delta,
			}); err != nil {
				return err
			}
		}
		it.pendingArgs = nil
		return nil
	}

	processToolCalls := func(rawCalls []any, deltas []chatToolCall) error {
		for i, raw := range rawCalls {
			var tc chatToolCall
			if i < len(deltas) {
				tc = deltas[i]
			}
			idx := -1
			if m, ok := raw.(map[string]any); ok {
				if v, ok := m["index"].(float64); ok {
					idx = int(v)
				}
			}
			if idx < 0 {
				switch {
				case tc.ID != "":
					if seen, ok := toolIDIndex[tc.ID]; ok {
						idx = seen
					} else if lastToolItem != nil && lastToolItem.callID == "" {
						// Late id: the previous fragment already opened the call.
						idx = lastToolItem.upstreamIndex
					} else {
						idx = nextToolIndex()
					}
				case lastToolItem != nil:
					idx = lastToolItem.upstreamIndex
				default:
					idx = nextToolIndex()
				}
			}

			it := toolByIndex[idx]
			if it == nil {
				it = newItem("function_call")
				it.id = "fc_" + randomHex(8)
				it.upstreamIndex = idx
				toolByIndex[idx] = it
			}
			lastToolItem = it
			if tc.ID != "" {
				if it.callID == "" {
					it.callID = tc.ID
				}
				toolIDIndex[tc.ID] = idx
			}
			if tc.Name != "" {
				it.name = mergeToolCallName(it.name, tc.Name)
			}
			if tc.Arguments != "" {
				it.args.WriteString(tc.Arguments)
				if it.added {
					if err := writeEvent("response.function_call_arguments.delta", map[string]any{
						"item_id":      it.id,
						"output_index": it.index,
						"delta":        tc.Arguments,
					}); err != nil {
						return err
					}
				} else {
					it.pendingArgs = append(it.pendingArgs, tc.Arguments)
				}
			}
			// A call needs both a name and a call id before it is addressable to
			// clients; wait for both (or finish) so added never carries empty ids.
			if it.name != "" && it.callID != "" && !it.added {
				if err := ensureToolAdded(it); err != nil {
					return err
				}
			}
		}
		return nil
	}

	validEvents := 0
	sawDone := false
	var usage map[string]any
	failureSent := false
	abort := errors.New("responses stream aborted")

	emitFailure := func(message, errType, code string) error {
		if message == "" {
			message = "upstream stream failed"
		}
		if errType == "" {
			errType = "upstream_error"
		}
		if code == "" {
			code = "upstream_error"
		}
		resp := baseResponse("failed")
		resp["error"] = map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		}
		if err := writeEvent("response.failed", map[string]any{"response": resp}); err != nil {
			return err
		}
		return writeDone()
	}

	if err := writeEvent("response.created", map[string]any{"response": baseResponse("in_progress")}); err != nil {
		return err
	}
	if err := writeEvent("response.in_progress", map[string]any{"response": baseResponse("in_progress")}); err != nil {
		return err
	}

	var scanErr error
	if rc != nil {
		defer rc.Close()
		sawDone, scanErr = upstream.ScanSSEWithDone(rc, func(payload string) error {
			var chunk map[string]any
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				// Non-JSON data (keepalive noise) is not a usable upstream event.
				return nil
			}
			validEvents++
			if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
				usage = u
			}
			if e, ok := chunk["error"]; ok && e != nil {
				message, errType, code := responsesStreamError(e)
				failureSent = true
				if err := emitFailure(message, errType, code); err != nil {
					return err
				}
				return abort
			}

			delta := firstDelta(chunk)
			if delta != nil {
				if err := handleReasoning(stringField(delta["reasoning_content"])); err != nil {
					return err
				}
				if err := handleContent(stringField(delta["content"])); err != nil {
					return err
				}
				if rawCalls, ok := delta["tool_calls"].([]any); ok && len(rawCalls) > 0 {
					if err := processToolCalls(rawCalls, toolCallDeltas(delta)); err != nil {
						return err
					}
				}
			}
			// Some providers send a full non-delta message instead of deltas.
			if msg := firstChoiceMessage(chunk); msg != nil && !deltaHasPayload(delta) {
				if err := handleReasoning(stringField(msg["reasoning_content"])); err != nil {
					return err
				}
				if err := handleContent(stringField(msg["content"])); err != nil {
					return err
				}
				if rawCalls, ok := msg["tool_calls"].([]any); ok && len(rawCalls) > 0 {
					if err := processToolCalls(rawCalls, toolCalls(msg)); err != nil {
						return err
					}
				}
			}

			return nil
		})
	}

	if failureSent {
		if errors.Is(scanErr, abort) {
			return errResponsesStreamFailed
		}
		return scanErr
	}
	if scanErr != nil {
		if err := emitFailure(scanErr.Error(), "upstream_error", "upstream_parse"); err != nil {
			return err
		}
		return errResponsesStreamFailed
	}
	if validEvents == 0 {
		if err := emitFailure("empty upstream stream", "upstream_error", "upstream_parse"); err != nil {
			return err
		}
		return errResponsesStreamFailed
	}
	if !sawDone {
		if err := emitFailure("upstream stream ended before completion", "upstream_error", "upstream_truncated"); err != nil {
			return err
		}
		return errResponsesStreamFailed
	}

	// Emit any delayed tool item headers before the done-event pass. Fully
	// malformed calls (no name and no call id) are dropped rather than emitted
	// with empty fields.
	for _, it := range items {
		if it.kind == "function_call" && !it.added {
			if it.name == "" && it.callID == "" {
				continue
			}
			if err := ensureToolAdded(it); err != nil {
				return err
			}
		}
	}

	output := make([]any, 0, len(items))
	for _, it := range items {
		switch it.kind {
		case "reasoning":
			text := it.text.String()
			if err := writeEvent("response.reasoning_summary_text.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"summary_index": 0,
				"text":          text,
			}); err != nil {
				return err
			}
			if err := writeEvent("response.reasoning_summary_part.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"summary_index": 0,
				"part":          map[string]any{"type": "summary_text", "text": text},
			}); err != nil {
				return err
			}
			item := map[string]any{
				"id":      it.id,
				"type":    "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": text}},
			}
			if err := writeEvent("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item":         item,
			}); err != nil {
				return err
			}
			output = append(output, item)
		case "message":
			text := it.text.String()
			if err := writeEvent("response.output_text.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"content_index": 0,
				"text":          text,
			}); err != nil {
				return err
			}
			if err := writeEvent("response.content_part.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			}); err != nil {
				return err
			}
			item := map[string]any{
				"id":      it.id,
				"type":    "message",
				"role":    "assistant",
				"status":  "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			}
			if err := writeEvent("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item":         item,
			}); err != nil {
				return err
			}
			output = append(output, item)
		case "function_call":
			if !it.added {
				continue // fully malformed call: nothing addressable to emit
			}
			arguments := it.args.String()
			if err := writeEvent("response.function_call_arguments.done", map[string]any{
				"item_id":      it.id,
				"output_index": it.index,
				"arguments":    arguments,
			}); err != nil {
				return err
			}
			item := map[string]any{
				"id":        it.id,
				"type":      "function_call",
				"call_id":   it.callID,
				"name":      it.name,
				"arguments": arguments,
				"status":    "completed",
			}
			if err := writeEvent("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item":         item,
			}); err != nil {
				return err
			}
			output = append(output, item)
		}
	}

	completed := baseResponse("completed")
	completed["output"] = output
	completed["usage"] = responsesUsage(usage)
	if err := writeEvent("response.completed", map[string]any{"response": completed}); err != nil {
		return err
	}
	return writeDone()
}

// mergeToolCallName folds one upstream name fragment into the accumulated tool
// name. Upstreams either send the name once, repeat the full name on every
// frame, or split it across frames; all three must yield the same final name.
func mergeToolCallName(current, next string) string {
	switch {
	case next == "":
		return current
	case current == "":
		return next
	case next == current:
		return current
	case strings.HasPrefix(next, current) && len(next) > len(current):
		return next
	case strings.HasPrefix(current, next):
		return current
	case strings.HasSuffix(current, next):
		return current
	case strings.HasSuffix(next, current) && len(next) > len(current):
		return next
	default:
		return current + next
	}
}

// deltaHasPayload reports whether a delta already carried usable content, so a
// non-delta message frame in the same chunk is not applied twice.
func deltaHasPayload(delta map[string]any) bool {
	if delta == nil {
		return false
	}
	if stringField(delta["content"]) != "" || stringField(delta["reasoning_content"]) != "" {
		return true
	}
	if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	return false
}

// responsesStreamError extracts the message/type/code trio from an upstream
// error value, tolerating string, object, numeric-code and nested data shapes.
func responsesStreamError(v any) (message, errType, code string) {
	switch e := v.(type) {
	case string:
		message = e
	case map[string]any:
		message = firstNonEmpty(stringField(e["message"]), stringField(e["msg"]))
		errType = stringField(e["type"])
		code = stringField(e["code"])
		if data, ok := e["data"].(map[string]any); ok {
			if message == "" {
				message = firstNonEmpty(stringField(data["message"]), stringField(data["msg"]))
			}
			if errType == "" {
				errType = stringField(data["type"])
			}
			if code == "" {
				code = stringField(data["code"])
			}
		}
		if nested, ok := e["message"].(map[string]any); ok {
			if message == "" {
				message = firstNonEmpty(stringField(nested["message"]), stringField(nested["msg"]))
			}
			if errType == "" {
				errType = stringField(nested["type"])
			}
			if code == "" {
				code = stringField(nested["code"])
			}
		}
	}
	if message == "" {
		message = "upstream stream failed"
	}
	if errType == "" {
		errType = "upstream_error"
	}
	if code == "" {
		code = "upstream_error"
	}
	return message, errType, code
}

// stringField converts string and numeric JSON values to a string; anything
// else (including objects) yields "".
func stringField(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
