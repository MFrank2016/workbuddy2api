// searchpick_test.go pickForSearch 的 realm / profile / 限流键判据（契约 §4）。
//
// 单独成文件：这些断言是 Todo 27 的 MCP tools/call **必须复用**的同一份逻辑，
// 与 HTTP 端点流程的测试分开读，复用面改动时一眼能定位到影响范围。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPickForSearchRateKey 限流键 = SHA-256(Authorization token)，客户端自造头无效。
func TestPickForSearchRateKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/search", nil)
	req.Header.Set("Authorization", "Bearer sk-search")
	req.Header.Set("X-WB2A-Rate-Key", "client-chosen")
	key := searchKey()
	pick, err := pickForSearch(req, &key)
	if err != nil {
		t.Fatalf("pickForSearch err=%v", err)
	}
	sum := sha256.Sum256([]byte("sk-search"))
	if want := hex.EncodeToString(sum[:]); pick.RateKey != want {
		t.Errorf("RateKey=%q want sha256(token)=%q", pick.RateKey, want)
	}
	if pick.Realm != "cn" || pick.Profile != "" || len(pick.Groups) != 0 {
		t.Errorf("pick=%+v want realm=cn profile empty groups empty", pick)
	}
}

// TestPickForSearchSingleProfile 单一 profile 的 key 在无 header 时推导 profile 与 realm。
func TestPickForSearchSingleProfile(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/search", nil)
	req.Header.Set("Authorization", "Bearer sk-search")
	key := AuthKey{Key: "sk-search", Profiles: []string{"intl-cli"}}
	pick, err := pickForSearch(req, &key)
	if err != nil {
		t.Fatalf("pickForSearch err=%v", err)
	}
	if pick.Realm != "global" || pick.Profile != "intl-cli" {
		t.Errorf("pick=%+v want realm=global profile=intl-cli", pick)
	}
}

// TestPickForSearchStarProfile "*" 允许任意 profile 且不贡献 realm。
func TestPickForSearchStarProfile(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/search", nil)
	req.Header.Set("Authorization", "Bearer sk-search")
	req.Header.Set("X-WB2A-Profile", "intl-work")
	key := AuthKey{Key: "sk-search", Profiles: []string{"*"}}
	pick, err := pickForSearch(req, &key)
	if err != nil {
		t.Fatalf("pickForSearch err=%v", err)
	}
	if pick.Realm != "global" || pick.Profile != "intl-work" {
		t.Errorf("pick=%+v want realm=global profile=intl-work", pick)
	}
}

// TestSearchRealmMismatch cn 绑定的 key 发 X-WB2A-Realm: global → 400 key_realm_mismatch。
func TestSearchRealmMismatch(t *testing.T) {
	h := searchHandler(t, nil)
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, map[string]string{"X-WB2A-Realm": "global"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "key_realm_mismatch")
}

// TestSearchRealmHeaderFallback 头缺失/为空 → 回落 key.realm（cn key 选中 cn 号）。
func TestSearchRealmHeaderFallback(t *testing.T) {
	for _, header := range []string{"", " "} {
		var hits []searchHit
		h := searchHandler(t, &hits)
		rec := doSearch(t, h, "sk-search", `{"query":"x"}`, map[string]string{"X-WB2A-Realm": header})
		if rec.Code != 200 {
			t.Fatalf("header=%q code=%d want 200 body=%s", header, rec.Code, rec.Body)
		}
		if hits[0].authz != "Bearer at_cn" {
			t.Errorf("header=%q authz=%q want cn account", header, hits[0].authz)
		}
	}
}

// TestSearchProfileNotAllowed 头 profile 不在 key.profiles 允许集内 → 400 profile_not_allowed。
func TestSearchProfileNotAllowed(t *testing.T) {
	h := searchHandler(t, nil, AuthKey{Key: "sk-search", Name: "search", Profiles: []string{"cn-cli"}})
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, map[string]string{"X-WB2A-Profile": "intl-cli"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "profile_not_allowed")
}

// TestSearchRealmRequired key.profiles 跨 realm 且无 header profile / realm → 400 realm_required。
func TestSearchRealmRequired(t *testing.T) {
	h := searchHandler(t, nil, AuthKey{Key: "sk-search", Name: "search", Profiles: []string{"cn-cli", "intl-cli"}})
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "realm_required")
}

// TestSearchRealmFromHeaderProfile header profile 优先于 key.realm/profiles 推导 realm：
// intl-cli → global，选中 global 账号。
func TestSearchRealmFromHeaderProfile(t *testing.T) {
	var hits []searchHit
	h := searchHandler(t, &hits, AuthKey{Key: "sk-search", Name: "search", Profiles: []string{"cn-cli", "intl-cli"}})
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, map[string]string{"X-WB2A-Profile": "intl-cli"})
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if hits[0].authz != "Bearer at_gl" {
		t.Errorf("authz=%q want global account token (intl-cli derives global)", hits[0].authz)
	}
}

// TestSearchEmptyProfilesDefaultsCN 空 profiles / 无 realm → cn。
func TestSearchEmptyProfilesDefaultsCN(t *testing.T) {
	var hits []searchHit
	h := searchHandler(t, &hits, AuthKey{Key: "sk-search", Name: "search"})
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if hits[0].authz != "Bearer at_cn" {
		t.Errorf("authz=%q want cn account (empty profiles → cn)", hits[0].authz)
	}
}

// TestPickForSearchKeyRealmWinsOverProfiles 契约 §4 链：key.realm 优先于 profiles 隐含 realm。
// 不一致配置（realm=cn 但 profiles 只允许 intl-cli）不得让请求被路由到 global ——
// 否则一个 realm 绑定的密钥就能靠 profiles 绕过域绑定。
func TestPickForSearchKeyRealmWinsOverProfiles(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/search", nil)
	req.Header.Set("Authorization", "Bearer sk-search")
	key := AuthKey{Key: "sk-search", Realm: "cn", Profiles: []string{"intl-cli"}}
	pick, err := pickForSearch(req, &key)
	if err != nil {
		t.Fatalf("pickForSearch err=%v (key.realm must win over profiles realm)", err)
	}
	if pick.Realm != "cn" {
		t.Errorf("Realm=%q want cn (key.realm precedes profiles)", pick.Realm)
	}
}

// TestSearchProfileCannotBypassRealmBoundKey header profile 推出的 realm 与 key.realm
// 冲突 → 400 key_realm_mismatch（profile 不得绕过密钥域绑定）。
func TestSearchProfileCannotBypassRealmBoundKey(t *testing.T) {
	h := searchHandler(t, nil, AuthKey{
		Key: "sk-search", Name: "search", Realm: "cn",
		Profiles: []string{"cn-cli", "intl-cli"},
	})
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, map[string]string{"X-WB2A-Profile": "intl-cli"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "key_realm_mismatch")
}
