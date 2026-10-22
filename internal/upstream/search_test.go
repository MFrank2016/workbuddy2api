package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// searchRequestCapture 一次出站搜索请求的捕获结果（httptest 服务端视角）。
type searchRequestCapture struct {
	method  string
	path    string
	headers http.Header
	body    map[string]any
}

// newSearchServer 启动一个 httptest 上游，捕获请求并按 status/body 应答。
func newSearchServer(t *testing.T, status int, respBody string, got *searchRequestCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.headers = r.Method, r.URL.Path, r.Header.Clone()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read search body: %v", err)
		}
		got.body = nil
		if err := json.Unmarshal(raw, &got.body); err != nil {
			t.Errorf("search body is not JSON: %v (%s)", err, raw)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// searchClient 构造打到 httptest 上游的客户端（base 可覆盖）。
func searchClient(srv *httptest.Server) *Client {
	return &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
}

const searchTwoResults = `{"query":"rust edition","type":"text2text","provider":"1",
	"results":[
		{"title":"Rust Edition Guide","url":"https://doc.rust-lang.org/edition-guide/","snippet":"Choose between editions.","site":"doc.rust-lang.org"},
		{"title":"Edition - Rust By Example","url":"https://doc.rust-lang.org/rust-by-example/","snippet":"Rust by Example."}
	],"total_results":2,"response_time_ms":42}`

// TestSearchRequestShape 断言出站形状：方法 POST、精确路径、账号头、body 字段
// （query / type=text2text / max_results）与两条结果的成功解析。
func TestSearchRequestShape(t *testing.T) {
	var got searchRequestCapture
	srv := newSearchServer(t, http.StatusOK, searchTwoResults, &got)

	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1", Domain: "www.codebuddy.cn"}
	results, err := searchClient(srv).Search(a, "rust edition", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/agenttool/v1/search" {
		t.Errorf("path = %q, want /agenttool/v1/search", got.path)
	}
	// 账号头：复用 ChatHeaders 的账号出站头组，不是另行拼装。
	if h := got.headers.Get("Authorization"); h != "Bearer at" {
		t.Errorf("Authorization = %q, want Bearer at", h)
	}
	if h := got.headers.Get("X-User-Id"); h != "u1" {
		t.Errorf("X-User-Id = %q, want u1", h)
	}
	if h := got.headers.Get("X-Enterprise-Id"); h != "e1" {
		t.Errorf("X-Enterprise-Id = %q, want e1", h)
	}
	if h := got.headers.Get("X-CodeBuddy-Request"); h != "1" {
		t.Errorf("X-CodeBuddy-Request = %q, want 1", h)
	}
	if h := got.headers.Get("User-Agent"); !strings.HasPrefix(h, "WorkBuddy/") {
		t.Errorf("User-Agent = %q, want WorkBuddy/... desktop UA", h)
	}
	if h := got.headers.Get("Content-Type"); h != "application/json" {

		t.Errorf("Content-Type = %q, want application/json", h)

	}

	// 搜索是非流式端点：Accept 必须是 application/json（不是 chat 的 SSE 值）。

	if h := got.headers.Get("Accept"); h != "application/json" {

		t.Errorf("Accept = %q, want application/json", h)

	}

	// body 形状（参考实现 request_body）：query + type=text2text + max_results。
	if q, _ := got.body["query"].(string); q != "rust edition" {
		t.Errorf("body.query = %v, want rust edition", got.body["query"])
	}
	if typ, _ := got.body["type"].(string); typ != "text2text" {
		t.Errorf("body.type = %v, want text2text", got.body["type"])
	}
	if n, _ := got.body["max_results"].(float64); n != 2 {
		t.Errorf("body.max_results = %v, want 2", got.body["max_results"])
	}

	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Title != "Rust Edition Guide" ||
		results[0].URL != "https://doc.rust-lang.org/edition-guide/" ||
		results[0].Snippet != "Choose between editions." {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].URL != "https://doc.rust-lang.org/rust-by-example/" {
		t.Errorf("results[1] = %+v", results[1])
	}
}

// TestSearchClampsMaxResults max_results 缺省/越界按参考实现夹到 [1,10]。
func TestSearchClampsMaxResults(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want float64
	}{
		{in: 0, want: 5},   // 缺省 → DEFAULT_RESULTS
		{in: -3, want: 5},  // 非法 → 缺省
		{in: 99, want: 10}, // 超过上限 → MAX_RESULTS
	} {
		var got searchRequestCapture
		srv := newSearchServer(t, http.StatusOK, searchTwoResults, &got)
		if _, err := searchClient(srv).Search(&auth.Auth{AccessToken: "at", UID: "u1"}, "q", tc.in); err != nil {
			t.Fatalf("max_results=%d: %v", tc.in, err)
		}
		if n, _ := got.body["max_results"].(float64); n != tc.want {
			t.Errorf("max_results(%d) sent = %v, want %v", tc.in, got.body["max_results"], tc.want)
		}
	}
}

// TestSearchGlobalAccountUsesGlobalBase global 账号走 global chat base + 国际版 UA/X-Domain
// （与 chat 同一条路由/头机制，Todo 21 换 base 时本方法自动跟随）。
func TestSearchGlobalAccountUsesGlobalBase(t *testing.T) {
	var got searchRequestCapture
	srv := newSearchServer(t, http.StatusOK, searchTwoResults, &got)

	c := &Client{HTTP: srv.Client(), ChatBaseCN: "https://cn.invalid", ChatBaseGlobal: srv.URL, GlobalEnabled: true}
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	if _, err := c.Search(a, "q", 1); err != nil {
		t.Fatalf("search: %v", err)
	}
	if got.path != "/agenttool/v1/search" {
		t.Errorf("path = %q", got.path)
	}
	if ua := got.headers.Get("User-Agent"); !strings.Contains(ua, "WorkBuddy AI") {
		t.Errorf("global UA = %q, want WorkBuddy AI platform segment", ua)
	}
	if d := got.headers.Get("X-Domain"); d != "www.workbuddy.ai" {
		t.Errorf("global X-Domain = %q, want www.workbuddy.ai", d)
	}
}

// TestSearchServerErrorIsTyped 5xx → *Error 带 HTTP 状态码（与 chat 客户端同一错误信封）。
func TestSearchServerErrorIsTyped(t *testing.T) {
	var got searchRequestCapture
	srv := newSearchServer(t, http.StatusInternalServerError, `{"code":15003,"msg":"upstream boom"}`, &got)

	_, err := searchClient(srv).Search(&auth.Auth{AccessToken: "at", UID: "u1"}, "q", 1)
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", ue.Status)
	}
	if ue.Kind != ErrServer {
		t.Errorf("kind = %v, want server", ue.Kind)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error text %q must carry the status code", err.Error())
	}
}

// TestSearchClientErrorIsTyped 4xx → *Error 带状态码且不重试/不换号（客户端错误确定性失败）。
func TestSearchClientErrorIsTyped(t *testing.T) {
	var got searchRequestCapture
	srv := newSearchServer(t, http.StatusBadRequest, `{"code":15003,"msg":"synthetic rejection"}`, &got)

	_, err := searchClient(srv).Search(&auth.Auth{AccessToken: "at", UID: "u1"}, "q", 1)
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", ue.Status)
	}
	if ue.Kind != ErrClient {
		t.Errorf("kind = %v, want client", ue.Kind)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error text %q must carry the status code", err.Error())
	}
}

// TestSearchBusinessCodeError 200 但业务 code 非 0 → *Error（不把错误当空结果吞掉）。
func TestSearchBusinessCodeError(t *testing.T) {
	var got searchRequestCapture
	srv := newSearchServer(t, http.StatusOK, `{"code":15003,"msg":"search rejected"}`, &got)

	_, err := searchClient(srv).Search(&auth.Auth{AccessToken: "at", UID: "u1"}, "q", 1)
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if !strings.Contains(ue.Msg, "15003") {
		t.Errorf("msg = %q, want business code", ue.Msg)
	}
}

// TestSearchDropsResultsWithoutURL 参考实现过滤掉无 url 的条目（不可点击的结果无意义）。
func TestSearchDropsResultsWithoutURL(t *testing.T) {
	var got searchRequestCapture
	srv := newSearchServer(t, http.StatusOK, `{"results":[
		{"title":"no url","snippet":"x"},
		{"title":"ok","url":"https://example.com/","snippet":"y"}
	]}`, &got)

	results, err := searchClient(srv).Search(&auth.Auth{AccessToken: "at", UID: "u1"}, "q", 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].URL != "https://example.com/" {
		t.Errorf("results = %+v, want only the item with a url", results)
	}
}
