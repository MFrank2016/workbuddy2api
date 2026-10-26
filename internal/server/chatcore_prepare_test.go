package server

import (
	"context"
	"net/http"
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

func TestPrepareChatRequestRejections(t *testing.T) {
	chatBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name           string
		body           string
		key            *AuthKey
		hidden         []string
		maxImages      int
		imagePolicy    string
		wantStatus     int
		wantCode       string
		wantImageCount int
		wantMaxImages  int
	}{
		{
			name:       "invalid json",
			body:       `{"model":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "key realm mismatch",
			body:       chatBody,
			key:        &AuthKey{Realm: "global"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "key_realm_mismatch",
		},
		{
			name:       "key model not allowed",
			body:       chatBody,
			key:        &AuthKey{Models: []string{"other-*"}},
			wantStatus: http.StatusBadRequest,
			wantCode:   "model_not_allowed",
		},
		{
			name:       "hidden model",
			body:       chatBody,
			hidden:     []string{"glm-5.2"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "model_unavailable",
		},
		{
			name:           "image policy error",
			body:           imagesChatBody("glm-5.2", 17, "img"),
			maxImages:      16,
			imagePolicy:    "error",
			wantStatus:     http.StatusRequestEntityTooLarge,
			wantCode:       "too_many_images",
			wantImageCount: 17,
			wantMaxImages:  16,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(Config{
				Upstream:     newFakeUpstream(t, func(string) (int, string, bool) { return 200, "", false }),
				ModelsHidden: tc.hidden,
				MaxImages:    tc.maxImages,
				ImagePolicy:  tc.imagePolicy,
			})
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tc.key != nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyAuthKey{}, tc.key))
			}
			_, err := h.prepareChatRequest(r, ChatExecOptions{Body: []byte(tc.body)})
			if err == nil {
				t.Fatal("expected preflight rejection")
			}
			if err.Status != tc.wantStatus || err.Code != tc.wantCode {
				t.Fatalf("got status=%d code=%q, want status=%d code=%q", err.Status, err.Code, tc.wantStatus, tc.wantCode)
			}
			if err.ImageCount != tc.wantImageCount || err.MaxImages != tc.wantMaxImages {
				t.Fatalf("got imageCount=%d maxImages=%d, want %d/%d", err.ImageCount, err.MaxImages, tc.wantImageCount, tc.wantMaxImages)
			}
		})
	}
}
