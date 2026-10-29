package qr

import (
	"strings"
	"testing"
)

func TestQRDataURI(t *testing.T) {
	uri, err := QRDataURI("https://example.com/auth?state=x", 240)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("uri=%q", uri)
	}
	if len(uri) < 100 {
		t.Fatalf("too short: %d", len(uri))
	}
}

func TestRenderTerminalQR(t *testing.T) {
	out, err := RenderTerminalQR("https://example.com/auth?state=x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "█") && !strings.Contains(out, "▀") {
		t.Fatalf("no block chars: %q", out)
	}
}
