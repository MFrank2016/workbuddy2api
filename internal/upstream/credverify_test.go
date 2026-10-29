package upstream

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestCredentialStillUsable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		keep   bool
	}{
		{"ok", 200, true},
		{"unauthorized", 401, false},
		{"forbidden-inconclusive", 403, true},
		{"server-inconclusive", 500, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					_, _ = io.WriteString(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`)
				}
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
			a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "copilot.tencent.com"}
			keep, _ := c.CredentialStillUsable(context.Background(), a)
			if keep != tc.keep {
				t.Fatalf("status=%d keep=%v want %v", tc.status, keep, tc.keep)
			}
		})
	}
}
