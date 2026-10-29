package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintLoginQR(t *testing.T) {
	var out bytes.Buffer
	printLoginQR(&out, "https://example.com/auth?state=x")
	got := out.String()
	if !strings.Contains(got, "█") && !strings.Contains(got, "▀") {
		t.Fatalf("no QR block chars in output: %q", got)
	}
	if !strings.Contains(got, "https://example.com/auth") {
		t.Fatalf("auth URL missing from output: %q", got)
	}
}
