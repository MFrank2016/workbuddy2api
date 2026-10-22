// search.go 上游「Web 搜索」接口：POST {chat base}/agenttool/v1/search。
//
// 逆向来源：codebuddy2api@a10bb17 的 app/web_search.py（request_body / parse_results）
// 与 app/site_routing.py 的 search_url_for_headers：
//
//	POST {profile chat base}/agenttool/v1/search
//	{"query": "...", "type": "text2text", "max_results": N}   // N 夹到 [1,10]，缺省 5
//	→ {"results":[{"title","url","snippet","site"}...], "total_results":N}
//
// 出站 base 用 chatBase(a)——与 chat 同一条 profile-aware 路由（Todo 21 改 chatBase
// 时本方法自动跟随，无需改动此处）；请求头复用 ChatHeaders（账号 Authorization/
// X-User-Id/X-Enterprise-Id/X-Domain/UA/Origin/Referer/设备头），线路与超时复用
// clientFor(a, false)。
//
// 边界：本文件只做 REST 搜索调用，**不实现 MCP 协议**（JSON-RPC 工具端点由 Todo 27
// 承载，它只调用 Search 并把结果渲染成工具文本）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// searchPath 上游搜索路径（参考实现 search_url_for_headers 逐字）。
const searchPath = "/agenttool/v1/search"

// 搜索条数口径（参考实现 web_search.DEFAULT_RESULTS / MAX_RESULTS）。
const (
	searchDefaultResults = 5
	searchMaxResults     = 10
)

// searchType 上游要求的检索类型（参考实现 request_body 固定值）。
const searchType = "text2text"

// SearchResult 一条搜索结果（上游条目只取三个稳定字段；site 等其余字段忽略）。
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// searchBody 出站请求体（字段名与参考实现一致）。
type searchBody struct {
	Query      string `json:"query"`
	Type       string `json:"type"`
	MaxResults int    `json:"max_results"`
}

// searchResponse 上游响应信封。results 在顶层（不在 data 里），故不能复用 doJSON
// 的 {code,msg,data} 解包；code 可能缺省、数字或字符串（参考实现接受 None/0/"0"）。
type searchResponse struct {
	Code    json.RawMessage `json:"code"`
	Msg     string          `json:"msg"`
	Results []SearchResult  `json:"results"`
}

// Search 执行一次上游 Web 搜索，返回解析后的结果条目。
//
// maxResults 缺省/越界按参考实现夹到 [1,10]（<=0 → 5，>10 → 10）。
// HTTP 非 2xx 返回带状态码的 *Error（Kind 由 Classify 分类，与 chat 客户端同一
// 错误信封，调用方可按 Status/Kind 决定换号或直接失败）；响应体不可解析返回
// 普通 error（上游脏数据，非账号问题）。
func (c *Client) Search(a *auth.Auth, query string, maxResults int) ([]SearchResult, error) {
	body, err := json.Marshal(searchBody{
		Query:      query,
		Type:       searchType,
		MaxResults: clampSearchResults(maxResults),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+searchPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 账号头一次成型：ChatHeaders 已含 CommonHeaders（Origin/Referer/UA/语言/设备头）
	// + Authorization/X-User-Id/企业与域头。
	// 会话头族与真实 chat 路径同族（上游渠道校验：X-Conversation-ID 缺头判非官方客户端
	// 400 code=11-128；handler 在轮转循环外用轮级聚合键兜底，见 handler.go：#170）。
	// search 无会话可聚合，故合成一次轮级键：X-Conversation-ID 用该键兜底、X-Trace-ID
	// 回落同键，单次请求内值稳定。参考实现 a10bb17 的 _web_search 同样带
	// X-Conversation-ID（_dynamic_request_headers，converter.py:545）。
	convReqID := session.NewMessageID()
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: convReqID, ConversationHeaderID: convReqID})
	// 搜索是非流式 JSON 端点：把 chat 的 SSE Accept 收敛回参考实现的 application/json。
	req.Header.Set("Accept", "application/json")
	// 按账号线路出站：搜索带的是账号 Bearer，走共享 transport 等于漏出宿主 IP。
	client, cerr := c.clientFor(a, false)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{
			Kind:   Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode,
			Msg:    truncate(string(raw), 200),
		}
	}
	var env searchResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse search response: %w (body: %s)", err, truncate(string(raw), 120))
	}
	// 200 + 业务 code 非 0：上游明确拒绝，不能当成「零结果」吞掉。
	if !searchCodeOK(env.Code) {
		return nil, &Error{
			Kind:   ErrClient,
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("code=%s msg=%s", env.Code, truncate(env.Msg, 160)),
		}
	}
	// 无 url 的条目不可点击，参考实现同样丢弃（parse_results）。
	out := make([]SearchResult, 0, len(env.Results))
	for _, r := range env.Results {
		if r.URL == "" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// clampSearchResults 把请求条数夹到上游接受区间（参考实现 max(1, min(n, 10))，
// 缺省 5；<=0 视为缺省——与参考实现的 `max_results or DEFAULT_RESULTS` 一致）。
func clampSearchResults(n int) int {
	if n <= 0 {
		return searchDefaultResults
	}
	if n > searchMaxResults {
		return searchMaxResults
	}
	return n
}

// searchCodeOK 判定响应信封业务码是否成功：缺省、null、0、"0" 均视为成功
// （参考实现 `code not in (None, 0, "0")` 才报错）。
func searchCodeOK(code json.RawMessage) bool {
	switch string(bytes.TrimSpace(code)) {
	case "", "null", "0", `"0"`:
		return true
	default:
		return false
	}
}
