package upstream

import (
	"crypto/tls"
	"net"
	"reflect"
	"testing"

	utls "github.com/refraction-networking/utls"
)

// buildHello 用官方 spec 构建 ClientHello（不实际握手），返回 uTLS 的 Hello。
func buildHello(t *testing.T) *utls.PubClientHelloMsg {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	uc := utls.UClient(client, &utls.Config{ServerName: "www.workbuddy.cn", MinVersion: tls.VersionTLS12}, utls.HelloCustom)
	if err := uc.ApplyPreset(officialHelloSpec()); err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	if err := uc.BuildHandshakeState(); err != nil {
		t.Fatalf("BuildHandshakeState: %v", err)
	}
	return uc.HandshakeState.Hello
}

// TestOfficialHelloSpecShape 官方 ClientHello 形态：18 cipher（TLS1.3 三件套在
// 最前）、10 扩展按真机顺序、9 sigalgs、groups [x25519,p256,p384]。
func TestOfficialHelloSpecShape(t *testing.T) {
	h := buildHello(t)
	if len(h.CipherSuites) != 18 {
		t.Fatalf("cipher count=%d want 18", len(h.CipherSuites))
	}
	wantCiphers := []uint16{
		tls.TLS_AES_128_GCM_SHA256, tls.TLS_AES_256_GCM_SHA384, tls.TLS_CHACHA20_POLY1305_SHA256,
	}
	if !reflect.DeepEqual(h.CipherSuites[:3], wantCiphers) {
		t.Fatalf("first ciphers=%v want TLS1.3 trio first", h.CipherSuites[:3])
	}
	if len(h.SupportedSignatureAlgorithms) != 9 {
		t.Fatalf("sigalgs=%d want 9", len(h.SupportedSignatureAlgorithms))
	}
	wantCurves := []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}
	if !reflect.DeepEqual(h.SupportedCurves, wantCurves) {
		t.Fatalf("curves=%v want %v", h.SupportedCurves, wantCurves)
	}
	if len(h.SupportedPoints) != 1 || h.SupportedPoints[0] != 0 {
		t.Fatalf("points=%v want [0]", h.SupportedPoints)
	}
}

// TestValidateTLSFingerprint 配置取值校验：空/go → go，node → node，其余报错。
func TestValidateTLSFingerprint(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"", TLSFingerprintGo, true},
		{"go", TLSFingerprintGo, true},
		{" NODE ", TLSFingerprintNode, true},
		{"chrome", "", false},
	} {
		got, err := validateTLSFingerprint(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("%q: err=%v ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("%q → %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestTLSFingerprintDefaultGo 默认必须是 go（保持改前行为），node 需显式开启。
func TestTLSFingerprintDefaultGo(t *testing.T) {
	if got := currentTLSFingerprint(); got != TLSFingerprintGo {
		t.Fatalf("default fingerprint=%q want go", got)
	}
	if useNodeFingerprint("") {
		t.Fatal("empty fingerprint must not enable node mode")
	}
}
