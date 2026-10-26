package server

import (
	"net/http/httptest"
	"testing"
)

func TestPrepareChatRequestModelAndRealm(t *testing.T) {
	h := NewHandler(Config{Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, "", false })})
	cases := []struct {
		name  string
		model string
		realm string
		bare  string
	}{
		{"bare", "glm-5.2", "cn", "glm-5.2"},
		{"cn-prefix", "cn:glm-5.2", "cn", "glm-5.2"},
		{"global-prefix", "global:glm-5.2", "global", "glm-5.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","messages":[{"role":"user","content":"hi"}]}`)
			p, err := h.prepareChatRequest(httptest.NewRequest("POST", "/v1/chat/completions", nil), ChatExecOptions{Body: body})
			if err != nil {
				t.Fatal(err)
			}
			if p.Realm != tc.realm || p.BareModel != tc.bare {
				t.Fatalf("got realm=%q bare=%q", p.Realm, p.BareModel)
			}
		})
	}
}
