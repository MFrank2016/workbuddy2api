// mcp_test.go Todo 27：无状态 MCP /v1/mcp 的协议、鉴权、限流与选号。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/reqlog"
	"workbuddy2api/internal/upstream"
)

func mcpHandler(t *testing.T, hits *[]searchHit, keys ...AuthKey) *Handler {
	t.Helper()
	if len(keys) == 0 {
		keys = []AuthKey{searchKey()}
	}
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "gl1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	return NewHandler(Config{
		Pool:       p,
		Upstream:   newSearchUpstream(t, hits),
		AuthKeys:   keys,
		MCPEnabled: true,
	})
}

func doMCP(t *testing.T, h *Handler, authz, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", bytes.NewReader([]byte(body)))
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

func mcpToolCall(id string, args map[string]any) string {
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": "web_search", "arguments": args},
	})
	return string(raw)
}

func TestMCPDisabledReturns404(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newSearchUpstream(t, nil), AuthKeys: []AuthKey{searchKey()}})
	if rec := doMCP(t, h, "sk-search", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, nil); rec.Code != 404 {
		t.Fatalf("with key: code=%d want 404", rec.Code)
	}
	if rec := doMCP(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, nil); rec.Code != 404 {
		t.Fatalf("anonymous: code=%d want 404", rec.Code)
	}
}

func TestMCPOriginForbidden(t *testing.T) {
	h := mcpHandler(t, nil)
	rec := doMCP(t, h, "sk-search", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403 body=%s", rec.Code, rec.Body)
	}
}

func TestMCPInvalidJSONRPC(t *testing.T) {
	h := mcpHandler(t, nil)
	for _, body := range []string{`not json`, `{"jsonrpc":"1.0","id":1,"method":"initialize"}`,
		`{"id":1,"method":"initialize"}`} {
		rec := doMCP(t, h, "sk-search", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s code=%d want 400", body, rec.Code)
		}
	}
}

func TestMCPInitializeNegotiatesProtocol(t *testing.T) {
	h := mcpHandler(t, nil)
	for _, version := range mcpProtocols {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + version + `"}}`
		rec := doMCP(t, h, "sk-search", body, nil)
		if rec.Code != 200 {
			t.Fatalf("version=%s code=%d body=%s", version, rec.Code, rec.Body)
		}
		var resp struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if resp.Result.ProtocolVersion != version {
			t.Errorf("requested %s got %s", version, resp.Result.ProtocolVersion)
		}
	}
	// 未知版本回落最新。
	rec := doMCP(t, h, "sk-search", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil)
	if !strings.Contains(rec.Body.String(), mcpDefaultProtocol) {
		t.Errorf("unknown version should fall back to %s: %s", mcpDefaultProtocol, rec.Body)
	}
}

func TestMCPToolsList(t *testing.T) {
	h := mcpHandler(t, nil)
	rec := doMCP(t, h, "sk-search", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Required []string `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0].Name != mcpToolName {
		t.Fatalf("tools=%+v want one %s", resp.Result.Tools, mcpToolName)
	}
	if len(resp.Result.Tools[0].InputSchema.Required) != 1 || resp.Result.Tools[0].InputSchema.Required[0] != "query" {
		t.Errorf("required=%v want [query]", resp.Result.Tools[0].InputSchema.Required)
	}
}

func TestMCPToolsCallSuccessAndLog(t *testing.T) {
	var hits []searchHit
	rec := reqlog.New(reqlog.Config{})
	h := mcpHandler(t, &hits)
	h.cfg.RequestLog = rec
	resp := doMCP(t, h, "sk-search", mcpToolCall("1", map[string]any{"query": "golang", "max_results": 3}), nil)
	if resp.Code != 200 {
		t.Fatalf("code=%d body=%s", resp.Code, resp.Body)
	}
	var out struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if out.Result.IsError || len(out.Result.Content) != 1 || out.Result.Content[0].Type != "text" {
		t.Fatalf("result=%+v want one text content", out.Result)
	}
	if !strings.Contains(out.Result.Content[0].Text, "T1") || !strings.Contains(out.Result.Content[0].Text, "https://e.x/1") {
		t.Errorf("content text missing results: %s", out.Result.Content[0].Text)
	}
	if len(hits) != 1 || hits[0].path != "/agenttool/v1/search" {
		t.Errorf("upstream hits=%+v want one search call", hits)
	}
	events := rec.Snapshot().Recent
	if len(events) != 1 || events[0].Model != "mcp" || events[0].Path != "/v1/mcp" || events[0].HasCredit {
		t.Errorf("reqlog events=%+v want model=mcp path=/v1/mcp has_credit=false", events)
	}
}

func TestMCPToolsCallProfileAndGroups(t *testing.T) {
	var hits []searchHit
	p := testPoolWith(
		&auth.Auth{UID: "cn-cli", AccessToken: "at_cncli", Domain: "copilot.tencent.com",
			Profile: "cn-cli", Groups: []string{"a"}, ExpiresAt: time.Now().Add(time.Hour).Unix()},
		&auth.Auth{UID: "cn-work", AccessToken: "at_cnwork", Domain: "www.workbuddy.cn",
			Profile: "cn-work", Groups: []string{"b"}, ExpiresAt: time.Now().Add(time.Hour).Unix()},
	)
	up := newSearchUpstream(t, &hits)
	up.ProfilesEnabled = true
	h := NewHandler(Config{
		Pool: p, Upstream: up, MCPEnabled: true,
		AuthKeys: []AuthKey{{Key: "sk", Name: "k", Groups: []string{"b"},
			Profiles: []string{"cn-cli", "cn-work"}}},
	})
	resp := doMCP(t, h, "sk", mcpToolCall("1", map[string]any{"query": "x"}),
		map[string]string{"X-WB2A-Profile": "cn-work"})
	if resp.Code != 200 {
		t.Fatalf("code=%d body=%s", resp.Code, resp.Body)
	}
	if len(hits) != 1 || hits[0].authz != "Bearer at_cnwork" {
		t.Errorf("hits=%+v want cn-work account (profile+groups)", hits)
	}
}

func TestMCPRealmAndProfileValidation(t *testing.T) {
	h := mcpHandler(t, nil, AuthKey{Key: "sk-search", Name: "search", Realm: "cn", Profiles: []string{"cn-cli"}})
	// cn key + global realm -> 400
	rec := doMCP(t, h, "sk-search", mcpToolCall("1", map[string]any{"query": "x"}),
		map[string]string{"X-WB2A-Realm": "global"})
	if rec.Code != 400 {
		t.Fatalf("realm mismatch code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "key_realm_mismatch")
	// profile outside key.Profiles -> 400
	rec = doMCP(t, h, "sk-search", mcpToolCall("1", map[string]any{"query": "x"}),
		map[string]string{"X-WB2A-Profile": "intl-cli"})
	if rec.Code != 400 {
		t.Fatalf("profile not allowed code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "profile_not_allowed")
}

func TestMCPUnknownMethodAndTool(t *testing.T) {
	h := mcpHandler(t, nil)
	rec := doMCP(t, h, "sk-search", `{"jsonrpc":"2.0","id":1,"method":"nope"}`, nil)
	if !strings.Contains(rec.Body.String(), "-32601") {
		t.Errorf("unknown method should return -32601: %s", rec.Body)
	}
	rec = doMCP(t, h, "sk-search", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`, nil)
	if !strings.Contains(rec.Body.String(), "-32602") {
		t.Errorf("unknown tool should return -32602: %s", rec.Body)
	}
}

func TestMCPNotificationAccepted(t *testing.T) {
	h := mcpHandler(t, nil)
	rec := doMCP(t, h, "sk-search", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d want 202", rec.Code)
	}
}

func TestMCPRateLimit601(t *testing.T) {
	h := mcpHandler(t, nil)
	body := mcpToolCall("1", map[string]any{"query": "x"})
	for i := 0; i < 600; i++ {
		if rec := doMCP(t, h, "sk-search", body, nil); rec.Code != 200 {
			t.Fatalf("request %d: code=%d body=%s", i+1, rec.Code, rec.Body)
		}
	}
	rec := doMCP(t, h, "sk-search", body, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("601st: code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After=%q want 60", got)
	}
}

func TestMCPUpstreamErrorIsTyped(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at_cn", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Header: http.Header{},
				Body: http.NoBody}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up, MCPEnabled: true,
		AuthKeys: []AuthKey{searchKey()}})
	rec := doMCP(t, h, "sk-search", mcpToolCall("1", map[string]any{"query": "x"}), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
}
