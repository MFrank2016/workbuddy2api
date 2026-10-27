package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/upstream"
)

// errAnthropicStreamFailed marks an in-band Anthropic stream failure: the
// terminal "event: error" frame was already written, and the caller should
// treat the request as failed (logging/account policy).
var errAnthropicStreamFailed = errors.New("anthropic stream failed")

// anthropicStreamBlock tracks one Anthropic content block while it is replayed
// from the upstream chat stream. Blocks keep a stable index assigned on first
// appearance so text/thinking/tool blocks stay ordered.
type anthropicStreamBlock struct {
	kind          string
	index         int
	upstreamIndex int
	id            string
	name          string
	args          strings.Builder
	pendingArgs   []string
	opened        bool
}

// streamAnthropic converts an upstream chat SSE stream into Anthropic Messages
// events. Success always terminates with exactly one message_stop. An upstream
// error frame, scan failure, empty stream or truncated stream emits an
// Anthropic error event, never message_stop, and returns errAnthropicStreamFailed.
func streamAnthropic(w http.ResponseWriter, rc io.ReadCloser, meta *anthropicMeta) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	write := func(event string, data any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	model := ""
	if meta != nil {
		model = meta.Model
	}

	msgID := "msg_" + randomHex(12)
	if err := write("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}

	blocks := []*anthropicStreamBlock{}
	newBlock := func(kind string) *anthropicStreamBlock {
		b := &anthropicStreamBlock{kind: kind, index: len(blocks), upstreamIndex: -1}
		blocks = append(blocks, b)
		return b
	}

	var textBlock, thinkingBlock *anthropicStreamBlock
	toolByIndex := map[int]*anthropicStreamBlock{}
	toolByID := map[string]int{}
	var lastTool *anthropicStreamBlock
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

	openBlock := func(b *anthropicStreamBlock) error {
		if b.opened {
			return nil
		}
		b.opened = true
		contentBlock := map[string]any{"type": b.kind}
		switch b.kind {
		case "text":
			contentBlock["text"] = ""
		case "thinking":
			contentBlock["thinking"] = ""
		case "tool_use":
			id := b.id
			if id == "" {
				id = "toolu_" + randomHex(12)
				b.id = id
			}
			contentBlock["id"] = id
			contentBlock["name"] = b.name
			contentBlock["input"] = map[string]any{}
		}
		if err := write("content_block_start", map[string]any{
			"type": "content_block_start", "index": b.index, "content_block": contentBlock,
		}); err != nil {
			return err
		}
		if b.kind == "tool_use" {
			for _, fragment := range b.pendingArgs {
				if err := write("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": b.index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": fragment},
				}); err != nil {
					return err
				}
			}
			b.pendingArgs = nil
		}
		return nil
	}

	appendText := func(text string) error {
		if text == "" {
			return nil
		}
		if textBlock == nil {
			textBlock = newBlock("text")
		}
		if err := openBlock(textBlock); err != nil {
			return err
		}
		return write("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": textBlock.index,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}
	appendThinking := func(text string) error {
		if text == "" {
			return nil
		}
		if thinkingBlock == nil {
			thinkingBlock = newBlock("thinking")
		}
		if err := openBlock(thinkingBlock); err != nil {
			return err
		}
		return write("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": thinkingBlock.index,
			"delta": map[string]any{"type": "thinking_delta", "thinking": text},
		})
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
					if seen, ok := toolByID[tc.ID]; ok {
						idx = seen
					} else if lastTool != nil && lastTool.id == "" {
						// Late id: the previous fragment already opened the call.
						idx = lastTool.upstreamIndex
					} else {
						idx = nextToolIndex()
					}
				case lastTool != nil:
					idx = lastTool.upstreamIndex
				default:
					idx = nextToolIndex()
				}
			}

			b := toolByIndex[idx]
			if b == nil {
				b = newBlock("tool_use")
				b.upstreamIndex = idx
				toolByIndex[idx] = b
			}
			lastTool = b
			if tc.ID != "" {
				if b.id == "" {
					b.id = tc.ID
				}
				toolByID[tc.ID] = idx
			}
			if tc.Name != "" {
				b.name = mergeToolCallName(b.name, tc.Name)
			}
			if tc.Arguments != "" {
				b.args.WriteString(tc.Arguments)
				if b.opened {
					if err := write("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": b.index,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Arguments},
					}); err != nil {
						return err
					}
				} else {
					b.pendingArgs = append(b.pendingArgs, tc.Arguments)
				}
			}
			// Open once the name is addressable. Name-only fragments keep
			// accumulating until arguments arrive or the stream finishes.
			if !b.opened && b.name != "" && (b.args.Len() > 0 || tc.Arguments != "") {
				if err := openBlock(b); err != nil {
					return err
				}
			}
		}
		return nil
	}

	validEvents := 0
	sawDone := false
	finishReason := ""
	var usage map[string]any
	failureSent := false
	abort := errors.New("anthropic stream aborted")

	emitFailure := func(message string) error {
		if message == "" {
			message = "upstream stream failed"
		}
		return write("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": message},
		})
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
			if reason := firstFinishReason(chunk); reason != "" {
				finishReason = reason
			}
			if e, ok := chunk["error"]; ok && e != nil {
				message, _, _ := responsesStreamError(e)
				failureSent = true
				if err := emitFailure(message); err != nil {
					return err
				}
				return abort
			}

			delta := firstDelta(chunk)
			if delta != nil {
				if err := appendThinking(stringField(delta["reasoning_content"])); err != nil {
					return err
				}
				if err := appendText(stringField(delta["content"])); err != nil {
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
				if err := appendThinking(stringField(msg["reasoning_content"])); err != nil {
					return err
				}
				if err := appendText(stringField(msg["content"])); err != nil {
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
		return errAnthropicStreamFailed
	}
	if scanErr != nil {
		if err := emitFailure(scanErr.Error()); err != nil {
			return err
		}
		return errAnthropicStreamFailed
	}
	if validEvents == 0 {
		if err := emitFailure("empty upstream stream"); err != nil {
			return err
		}
		return errAnthropicStreamFailed
	}
	if !sawDone {
		if err := emitFailure("upstream stream ended before completion"); err != nil {
			return err
		}
		return errAnthropicStreamFailed
	}

	// Flush any delayed tool headers, then close every block in index order.
	for _, b := range blocks {
		if b.opened {
			continue
		}
		if b.kind == "tool_use" && b.name == "" && b.id == "" && b.args.Len() == 0 {
			continue
		}
		if err := openBlock(b); err != nil {
			return err
		}
	}
	for _, b := range blocks {
		if !b.opened {
			continue
		}
		if err := write("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": b.index,
		}); err != nil {
			return err
		}
	}

	stop := anthropicStopReason(finishReason)
	if stop == "end_turn" && len(toolByIndex) > 0 {
		stop = "tool_use"
	}
	outputTokens := intFromAny(usage["completion_tokens"])
	if err := write("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	}); err != nil {
		return err
	}
	return write("message_stop", map[string]any{"type": "message_stop"})
}
