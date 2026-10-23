// profile_select_test.go Todo 22：请求 profile 解析、越界拒绝、选号与目录。
package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func profileChatUpstream(t *testing.T) *upstream.Client {
	t.Helper()
	return newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
}

func profileChatHandler(t *testing.T, enabled bool, keys ...AuthKey) (*Handler, *upstream.Client, *pool.Pool) {
	t.Helper()
	p := testPoolWith(
		&auth.Auth{UID: "cn-cli", AccessToken: "at_cncli", Domain: "copilot.tencent.com", Profile: "cn-cli", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		&auth.Auth{UID: "cn-work", AccessToken: "at_cnwork", Domain: "www.workbuddy.cn", Profile: "cn-work", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	)
	up := profileChatUpstream(t)
	up.ProfilesEnabled = enabled
	return NewHandler(Config{
		Pool:     p,
		Upstream: up,
		AuthKeys: keys,
	}), up, p
}

func doChat(t *testing.T, h *Handler, key string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatProfileHeaderSelectsAccount(t *testing.T) {
	h, _, p := profileChatHandler(t, true, AuthKey{Key: "sk", Name: "k", Profiles: []string{"cn-cli", "cn-work"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-WB2A-Profile", "cn-work")
	if got, perr := h.resolveRequestProfile(req, &AuthKey{Key: "sk", Profiles: []string{"cn-cli", "cn-work"}}); perr != nil || got != "cn-work" {
		t.Fatalf("resolveRequestProfile=%q err=%v want cn-work", got, perr)
	}
	if h.cfg.Pool != p {
		t.Fatal("handler pool is not the test pool")
	}
	if acct := p.PickExcludingForRealmGroupsProfiles(nil, "glm-5.2", "cn", nil, "cn-work"); acct == nil {
		t.Fatal("direct pool pick returned nil")
	} else {
		t.Logf("direct pool pick=%s", acct.UID)
	}
	rec := doChat(t, h, "sk", map[string]string{"X-WB2A-Profile": "cn-work"},
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
}

func TestChatProfileNotAllowed(t *testing.T) {
	h, _, _ := profileChatHandler(t, true, AuthKey{Key: "sk", Name: "k", Profiles: []string{"cn-cli"}})
	for _, header := range []string{"cn-work", "bogus"} {
		rec := doChat(t, h, "sk", map[string]string{"X-WB2A-Profile": header},
			`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("profile=%s code=%d want 400 body=%s", header, rec.Code, rec.Body)
		}
		assertJSONErrorCode(t, rec.Body.String(), "profile_not_allowed")
	}
}

func TestChatProfileRealmMismatch(t *testing.T) {
	h, _, _ := profileChatHandler(t, true, AuthKey{Key: "sk", Name: "k", Profiles: []string{"intl-cli"}})
	rec := doChat(t, h, "sk", map[string]string{"X-WB2A-Profile": "intl-cli"},
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "key_realm_mismatch")
}

func TestChatProfileIgnoredWhenFlagOff(t *testing.T) {
	// profiles 未激活：header 越界也不得 400，选号忽略 profile。
	h, _, _ := profileChatHandler(t, false, AuthKey{Key: "sk", Name: "k", Profiles: []string{"cn-cli"}})
	rec := doChat(t, h, "sk", map[string]string{"X-WB2A-Profile": "cn-work"},
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("flag off code=%d want 200 body=%s", rec.Code, rec.Body)
	}
}

func TestChatKeySingleProfileWithoutHeader(t *testing.T) {
	h, _, _ := profileChatHandler(t, true, AuthKey{Key: "sk", Name: "k", Profiles: []string{"cn-work"}})
	rec := doChat(t, h, "sk", nil,
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
}

func TestProfileCatalogIsPerProfile(t *testing.T) {
	h, _, _ := profileChatHandler(t, true)
	h.storeProfileModelInfos("intl-cli", []upstream.ModelInfo{
		{ID: "gpt-x", SupportsImages: true, SupportsImagesSet: true},
	})
	h.storeProfileModelInfos("intl-work", []upstream.ModelInfo{
		{ID: "gpt-x", SupportsImages: false, SupportsImagesSet: true},
	})
	if got := h.lookupModelCapability("global", "gpt-x", "intl-cli"); !got.InCatalog || !got.SupportsImages {
		t.Errorf("intl-cli cap=%+v want supports images", got)
	}
	if got := h.lookupModelCapability("global", "gpt-x", "intl-work"); !got.InCatalog || got.SupportsImages {
		t.Errorf("intl-work cap=%+v want explicit no-images", got)
	}
	// 未知 profile / 冷缓存 fail-open（不回落 realm 目录，也不误判不支持）。
	if got := h.lookupModelCapability("global", "gpt-x", "cn-cli"); got.InCatalog {
		t.Errorf("cold profile cache must fail-open, got %+v", got)
	}
}

func TestProfileCatalogHintUsesRealmOnly(t *testing.T) {
	// 目录缓存不影响 hint 的 realm 口径（hint 走 realm 目录；profile 只影响预检）。
	h, _, _ := profileChatHandler(t, true)
	if got := h.hintContext("cn", "glm-5.2", false); got.ModelInCatalog {
		t.Errorf("cold realm cache should not be in catalog: %+v", got)
	}
}

func TestProfileHeaderIsNotForwardedUpstream(t *testing.T) {
	// 出站头由 _upstream_headers 决定；这里只确保请求头的 profile 不会让选号崩溃。
	h, _, _ := profileChatHandler(t, true, AuthKey{Key: "sk", Name: "k", Profiles: []string{"cn-cli"}})
	rec := doChat(t, h, "sk", map[string]string{"X-WB2A-Profile": "cn-cli"},
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}
