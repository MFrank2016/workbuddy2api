package upstream

import (
	"bufio"
	"io"
	"strings"
)

// sseMaxLine is the largest single SSE line ScanSSE accepts. Upstream frames can
// be large (tool arguments, long content deltas), so the default bufio.Scanner
// limit (64 KiB) is not enough.
const sseMaxLine = 4 * 1024 * 1024

// ScanSSE reads Server-Sent Events from r and invokes fn with the payload of
// every data frame. Comment lines (":..."), blank lines and non-data fields are
// ignored, the "data:" prefix is stripped, and scanning stops at the terminal
// "[DONE]" frame without invoking fn for it.
func ScanSSE(r io.Reader, fn func(payload string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseMaxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil
		}
		if payload == "" {
			continue
		}
		if err := fn(payload); err != nil {
			return err
		}
	}
	return sc.Err()
}
