package upstream

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"workbuddy2api/internal/auth"
)

func makeTestJWT(sub, iss string) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "iss": iss})
	return "x." + base64.RawURLEncoding.EncodeToString(payload) + ".y"
}

func TestValidateRefreshedTokenRejectsUIDMismatch(t *testing.T) {
	old := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	if err := validateRefreshedToken(old, makeTestJWT("u2", "copilot.tencent.com")); err == nil {
		t.Fatal("must reject uid mismatch")
	}
}

func TestValidateRefreshedTokenRejectsRealmMismatch(t *testing.T) {
	old := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	if err := validateRefreshedToken(old, makeTestJWT("u1", "www.workbuddy.ai")); err == nil {
		t.Fatal("must reject realm mismatch")
	}
}

func TestValidateRefreshedTokenAllowsMatch(t *testing.T) {
	old := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	if err := validateRefreshedToken(old, makeTestJWT("u1", "copilot.tencent.com")); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefreshedTokenAllowsOpaque(t *testing.T) {
	old := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	if err := validateRefreshedToken(old, "opaque-token"); err != nil {
		t.Fatal(err)
	}
}
