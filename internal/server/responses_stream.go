package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// responsesStreamItem tracks one output item while its deltas are replayed from
// the upstream chat stream. Items are ordered by first appearance so output
// indexes stay stable for the whole stream.
type responsesStreamItem struct {
	kind      string
	index     int
	id        string
	callID    string
	name      strings.Builder
	text      strings.Builder
	args      strings.Builder
	added     bool
	partAdded bool
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

	toolByIndex := map[int]*responsesStreamItem{}
	toolIDIndex := map[string]int{}
	lastToolIdx := -1
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

	validEvents := 0
	sawFinish := false
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
		scanErr = upstream.ScanSSE(rc, func(payload string) error {
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
				if reasoning, _ := delta["reasoning_content"].(string); reasoning != "" {
					it, err := startReasoning()
					if err != nil {
						return err
					}
					it.text.WriteString(reasoning)
					if err := writeEvent("response.reasoning_summary_text.delta", map[string]any{
						"item_id":       it.id,
						"output_index":  it.index,
						"summary_index": 0,
						"delta":         reasoning,
					}); err != nil {
						return err
					}
				}
				if content, _ := delta["content"].(string); content != "" {
					it, err := startMessage()
					if err != nil {
						return err
					}
					it.text.WriteString(content)
					if err := writeEvent("response.output_text.delta", map[string]any{
						"item_id":       it.id,
						"output_index":  it.index,
						"content_index": 0,
						"delta":         content,
					}); err != nil {
						return err
					}
				}
				if rawCalls, ok := delta["tool_calls"].([]any); ok && len(rawCalls) > 0 {
					deltas := toolCallDeltas(delta)
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
								} else {
									idx = nextToolIndex()
								}
							case lastToolIdx >= 0:
								idx = lastToolIdx
							default:
								idx = nextToolIndex()
							}
						}
						lastToolIdx = idx
						if tc.ID != "" {
							toolIDIndex[tc.ID] = idx
						}

						it := toolByIndex[idx]
						if it == nil {
							it = newItem("function_call")
							it.id = "fc_" + randomHex(8)
							toolByIndex[idx] = it
						}
						if tc.ID != "" && it.callID == "" {
							it.callID = tc.ID
						}
						if tc.Name != "" {
							it.name.WriteString(tc.Name)
						}
						if !it.added {
							it.added = true
							if err := writeEvent("response.output_item.added", map[string]any{
								"output_index": it.index,
								"item": map[string]any{
									"id":        it.id,
									"type":      "function_call",
									"call_id":   it.callID,
									"name":      it.name.String(),
									"arguments": "",
									"status":    "in_progress",
								},
							}); err != nil {
								return err
							}
						}
						if tc.Arguments != "" {
							it.args.WriteString(tc.Arguments)
							if err := writeEvent("response.function_call_arguments.delta", map[string]any{
								"item_id":      it.id,
								"output_index": it.index,
								"delta":        tc.Arguments,
							}); err != nil {
								return err
							}
						}
					}
				}
			}

			if choices, ok := chunk["choices"].([]any); ok {
				for _, rawChoice := range choices {
					choice, _ := rawChoice.(map[string]any)
					if fr, _ := choice["finish_reason"].(string); fr != "" {
						sawFinish = true
					}
				}
			}
			return nil
		})
	}

	if failureSent {
		if errors.Is(scanErr, abort) {
			return nil
		}
		return scanErr
	}
	if scanErr != nil {
		if err := emitFailure(scanErr.Error(), "upstream_error", "upstream_parse"); err != nil {
			return err
		}
		return scanErr
	}
	if validEvents == 0 {
		const message = "empty upstream stream"
		if err := emitFailure(message, "upstream_error", "upstream_parse"); err != nil {
			return err
		}
		return errors.New(message)
	}
	if !sawFinish {
		const message = "upstream stream ended before completion"
		if err := emitFailure(message, "upstream_error", "upstream_truncated"); err != nil {
			return err
		}
		return errors.New(message)
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
				"name":      it.name.String(),
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

// responsesStreamError extracts the message/type/code trio from an upstream
// error value, tolerating both object and string shapes.
func responsesStreamError(v any) (message, errType, code string) {
	switch e := v.(type) {
	case map[string]any:
		message, _ = e["message"].(string)
		errType, _ = e["type"].(string)
		code, _ = e["code"].(string)
	case string:
		message = e
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
