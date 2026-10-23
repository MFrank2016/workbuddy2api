// search_test.go Todo 26：POST /v1/search 的鉴权/参数/realm/profile/限流/日志行为。
//
// 全部走真实 handler 面（httptest + fake upstream transport），只对上游 HTTP 打桩，
// 不 mock 被测代码。限流用例按契约真实打满 600 次再断言第 601 次 429。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/reqlog"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// searchHit 一次 fake 上游搜索调用的观测值。
type searchHit struct {
	path  string
	authz string
	body  map[string]any
}

// newSearchUpstream 返回一个记录调用并回固定两条结果的 fake upstream。
func newSearchUpstream(t *testing.T, hits *[]searchHit) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if hits != nil {
				*hits = append(*hits, searchHit{path: r.URL.Path, authz: r.Header.Get("Authorization"), body: body})
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"code":0,"results":[` +
						`{"title":"T1","url":"https://e.x/1","snippet":"S1"},` +
						`{"title":"T2","url":"https://e.x/2","snippet":"S2"}]}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
}

// searchKey 构造一条分组密钥（默认 cn realm）。
func searchKey() AuthKey {
	return AuthKey{Key: "sk-search", Name: "search", Realm: "cn"}
}

// doSearch 发一次 POST /v1/search。headers 为额外入站头（nil = 无）。
func doSearch(t *testing.T, h *Handler, authz, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/search", bytes.NewReader([]byte(body)))
	if authz != "" {
		req.Header.Set("Authorization", "Bearer "+authz)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// searchHandler 组装启用 search 的 handler（cn + global 各一号）。
func searchHandler(t *testing.T, hits *[]searchHit, keys ...AuthKey) *Handler {
	t.Helper()
	if len(keys) == 0 {
		keys = []AuthKey{searchKey()}
	}
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "gl1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	return NewHandler(Config{
		Pool:          p,
		Upstream:      newSearchUpstream(t, hits),
		AuthKeys:      keys,
		SearchEnabled: true,
	})
}

// TestSearchDisabledReturns404 search.enabled=false 时路径不存在：带不带 key 都是 404。
func TestSearchDisabledReturns404(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newSearchUpstream(t, nil), AuthKeys: []AuthKey{searchKey()}})

	if rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("with key: code=%d want 404 body=%s", rec.Code, rec.Body)
	}
	if rec := doSearch(t, h, "", `{"query":"x"}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("anonymous: code=%d want 404 (flag off must look like a missing route)", rec.Code)
	}
}

// TestSearchAuth 缺失/无效 key → 401。
func TestSearchAuth(t *testing.T) {
	h := searchHandler(t, nil)
	for _, c := range []struct{ name, authz string }{
		{"missing", ""},
		{"invalid", "wrong-key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := doSearch(t, h, c.authz, `{"query":"x"}`, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code=%d want 401 body=%s", rec.Code, rec.Body)
			}
			assertJSONErrorCode(t, rec.Body.String(), "invalid_api_key")
		})
	}
}

// TestSearchParamValidation query 为空 / max_results 越界 / body 非 JSON → 400。
func TestSearchParamValidation(t *testing.T) {
	cases := []struct{ name, body string }{
		{"empty body", ``},
		{"not json", `not json`},
		{"missing query", `{}`},
		{"blank query", `{"query":"   "}`},
		{"zero max_results", `{"query":"x","max_results":0}`},
		{"negative max_results", `{"query":"x","max_results":-1}`},
		{"too many max_results", `{"query":"x","max_results":11}`},
		{"non-string query", `{"query":123}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := searchHandler(t, nil)
			rec := doSearch(t, h, "sk-search", c.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
			}
			assertJSONErrorCode(t, rec.Body.String(), "invalid_request")
		})
	}
}

// TestSearchSuccess happy path：结果形状、上游 path/body、reqlog 事件。
func TestSearchSuccess(t *testing.T) {
	var hits []searchHit
	rec := reqlog.New(reqlog.Config{})
	h := searchHandler(t, &hits)
	h.cfg.RequestLog = rec

	resp := doSearch(t, h, "sk-search", `{"query":"golang","max_results":3}`, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", resp.Code, resp.Body)
	}
	var got struct {
		Object  string `json:"object"`
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v body=%s", err, resp.Body)
	}
	if got.Object != "search_result" {
		t.Errorf("object=%q want search_result", got.Object)
	}
	if len(got.Results) != 2 || got.Results[0].Title != "T1" || got.Results[0].URL != "https://e.x/1" || got.Results[0].Snippet != "S1" {
		t.Errorf("results=%+v want 2 parsed items", got.Results)
	}
	if len(hits) != 1 {
		t.Fatalf("upstream calls=%d want 1", len(hits))
	}
	if hits[0].path != "/agenttool/v1/search" {
		t.Errorf("path=%q want /agenttool/v1/search", hits[0].path)
	}
	if hits[0].body["query"] != "golang" || hits[0].body["type"] != "text2text" || hits[0].body["max_results"] != float64(3) {
		t.Errorf("upstream body=%v want query/type/max_results=3", hits[0].body)
	}
	if hits[0].authz != "Bearer at_cn" {
		t.Errorf("upstream authz=%q want cn account token", hits[0].authz)
	}
	// reqlog：模型名固定 web_search，HasCredit=false（search 不产生积分观测）。
	events := rec.Snapshot().Recent
	if len(events) != 1 {
		t.Fatalf("reqlog events=%d want 1", len(events))
	}
	e := events[0]
	if e.Model != "web_search" || e.HasCredit || e.Path != "/v1/search" || e.Status != 200 || !e.OK {
		t.Errorf("reqlog event=%+v want model=web_search has_credit=false path=/v1/search status=200", e)
	}
}

// TestSearchDefaultMaxResults max_results 缺省 → 上游收到 5。
func TestSearchDefaultMaxResults(t *testing.T) {
	var hits []searchHit
	h := searchHandler(t, &hits)
	if rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil); rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if hits[0].body["max_results"] != float64(5) {
		t.Errorf("max_results=%v want default 5", hits[0].body["max_results"])
	}
}

// TestSearchGroupsFilter key.groups 参与选号：只有 external 组账号可用。
func TestSearchGroupsFilter(t *testing.T) {
	var hits []searchHit
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Groups: []string{"internal"}, ExpiresAt: 9999999999},
		&auth.Auth{UID: "cn2", AccessToken: "at_cn2", Groups: []string{"external"}, ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      newSearchUpstream(t, &hits),
		AuthKeys:      []AuthKey{{Key: "sk-search", Name: "search", Groups: []string{"external"}}},
		SearchEnabled: true,
	})
	if rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil); rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if hits[0].authz != "Bearer at_cn2" {
		t.Errorf("authz=%q want external-group account", hits[0].authz)
	}
}

// TestSearchNoAccountReturns503 池里没有可用号 → 503 no_healthy_account。
func TestSearchNoAccountReturns503(t *testing.T) {
	h := NewHandler(Config{
		Pool:          testPoolWith(),
		Upstream:      newSearchUpstream(t, nil),
		AuthKeys:      []AuthKey{searchKey()},
		SearchEnabled: true,
	})
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "no_healthy_account")
}

// TestSearchIgnoresModelWhitelist 模型白名单不适用于 search。
func TestSearchIgnoresModelWhitelist(t *testing.T) {
	h := searchHandler(t, nil, AuthKey{Key: "sk-search", Name: "search", Models: []string{"only-this-model"}})
	if rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil); rec.Code != 200 {
		t.Fatalf("code=%d want 200 (model whitelist must not gate search) body=%s", rec.Code, rec.Body)
	}
}

// TestSearchUsageRecorded usage 记 web_search 桶（请求数 1，无 token）。
func TestSearchUsageRecorded(t *testing.T) {
	u := usage.New("")
	h := searchHandler(t, nil)
	h.cfg.Usage = u
	if rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil); rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	snap := u.Snapshot(1, nil)
	found := false
	for _, row := range snap.ByModel {
		if row.Key == "web_search" {
			found = true
			if row.Requests != 1 || row.TotalTokens != 0 {
				t.Errorf("usage row=%+v want requests=1 tokens=0", row)
			}
		}
	}
	if !found {
		t.Errorf("usage by_model 缺 web_search 行: %+v", snap.ByModel)
	}
}

// TestSearchRateLimit601 600/min 固定窗口：第 601 次 → 429 + Retry-After: 60。
func TestSearchRateLimit601(t *testing.T) {
	h := searchHandler(t, nil)
	for i := 0; i < 600; i++ {
		rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code=%d want 200 body=%s", i+1, rec.Code, rec.Body)
		}
	}
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("601st: code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q want 60", got)
	}
	assertJSONErrorCode(t, rec.Body.String(), "rate_limit_exceeded")
}

// TestSearchDisabledAccountReturns503 池里有号但全部 disabled → 503（不回落其它域）。
func TestSearchDisabledAccountReturns503(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", ExpiresAt: 9999999999})
	p.Disable("cn1", "test disabled")
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      newSearchUpstream(t, nil),
		AuthKeys:      []AuthKey{searchKey()},
		SearchEnabled: true,
	})
	rec := doSearch(t, h, "sk-search", `{"query":"x"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "no_healthy_account")
}
