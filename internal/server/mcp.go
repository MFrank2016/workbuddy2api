// mcp.go POST /v1/mcp 无状态 JSON-RPC 2.0 端点（契约 §4 / Todo 27）。
//
// 只实现三个方法：initialize / tools/list / tools/call，工具只有 web_search。
// 刻意**无状态**：不发 Mcp-Session-Id、不做服务端主动请求、不开 WebSocket，
// 每个请求都是独立的一次 HTTP 往返 —— 会话态会把网关变成有状态服务，而这里
// 只是把已有的搜索能力按 MCP 的线格式暴露出去。
//
// 鉴权/选号/限流与 /v1/search 完全同源：pickForSearch 负责 realm/profile 校验
// 与限流键，searchWithAccount 负责选号、刷新与冷却。MCP 不另造一套判据
// （契约 §4 明确要求 tools/call 复用同一 helper）。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/reqlog"
	"workbuddy2api/internal/upstream"
)

const (
	// mcpLogModel reqlog / usage 里的模型名（契约 §4 固定值）。
	mcpLogModel = "mcp"
	// mcpMaxBody JSON-RPC 请求体上限；一次 tools/call 只带 query 与条数。
	mcpMaxBody = 1 << 20
	// mcpDefaultProtocol 客户端未指定/不支持时的协议版本（取最新）。
	mcpDefaultProtocol = "2026-07-28"
	// mcpServerName serverInfo.name。
	mcpServerName = "workbuddy2api"
)

// mcpProtocols 支持的协议版本（契约 §4）。
var mcpProtocols = []string{"2026-07-28", "2025-11-25", "2025-06-18"}

// mcpToolName 唯一工具名。
const mcpToolName = "web_search"

// jsonRPCRequest 最小 JSON-RPC 2.0 请求信封。id 用 RawMessage 原样回显
// （数字/字符串/null 都可能出现，不能被 float64 改写）。
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// jsonRPCError JSON-RPC 错误对象。
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonRPCResponse JSON-RPC 响应信封。result / error 互斥。
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// gatedMCP mcp.enabled 闸在 withAuth 之外：关闭时回纯文本 404，与路由未注册
// 完全一致（不向未鉴权探测暴露端点存在性）。开关走 livecfg，热改即生效。
func (h *Handler) gatedMCP(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().MCPEnabled {
		http.NotFound(w, r)
		return
	}
	h.withAuth(h.mcp)(w, r)
}

// mcp POST /v1/mcp 主流程。
//
// Origin 头一律 403：MCP 规范要求服务端拒绝浏览器来源的跨域调用（DNS
// rebinding 防护）；网关是给本机/私有客户端用的，没有合法的浏览器调用方。
func (h *Handler) mcp(w http.ResponseWriter, r *http.Request) {
	obs := &responseObserver{ResponseWriter: w}
	start := time.Now()

	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		writeOpenAIError(obs, http.StatusForbidden, "forbidden",
			"Origin header is not allowed on this endpoint")
		return
	}

	if rec := h.cfg.RequestLog; rec != nil {
		rec.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			ms := time.Since(start).Milliseconds()
			if ms < 1 {
				ms = 1
			}
			rec.Record(reqlog.Event{
				Time:       start,
				Path:       "/v1/mcp",
				Model:      mcpLogModel,
				Status:     status,
				OK:         status >= 200 && status < 300,
				HasCredit:  false,
				DurationMs: ms,
			})
		}()
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody))
	if err != nil {
		writeJSON(obs, http.StatusBadRequest,
			jsonRPCResponse{JSONRPC: "2.0", Error: &jsonRPCError{Code: -32700, Message: "read body: " + err.Error()}})
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(obs, http.StatusBadRequest,
			jsonRPCResponse{JSONRPC: "2.0", Error: &jsonRPCError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
		writeJSON(obs, http.StatusBadRequest,
			jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &jsonRPCError{Code: -32600, Message: "invalid JSON-RPC 2.0 request"}})
		return
	}

	// 通知（无 id）不需要响应体：MCP 客户端会在 initialize 后发
	// notifications/initialized，回 202 空体即可。
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
			Result: h.mcpInitialize(req.Params)})
	case "tools/list":
		writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
			Result: map[string]any{"tools": []any{mcpWebSearchTool()}}})
	case "tools/call":
		h.mcpToolsCall(obs, r, req)
	case "notifications/initialized", "notifications/cancelled":
		if isNotification {
			obs.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	default:
		writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCError{Code: -32601, Message: "method not found: " + req.Method}})
	}
}

// mcpInitialize 协商协议版本并返回能力声明。
func (h *Handler) mcpInitialize(params json.RawMessage) map[string]any {
	requested := ""
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) == nil {
			requested = strings.TrimSpace(p.ProtocolVersion)
		}
	}
	version := mcpDefaultProtocol
	for _, v := range mcpProtocols {
		if v == requested {
			version = v
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": mcpServerName, "version": "1.0.0"},
	}
}

// mcpWebSearchTool tools/list 里的工具描述。
func mcpWebSearchTool() map[string]any {
	return map[string]any{
		"name":        mcpToolName,
		"description": "Search the web and return titles, URLs and snippets.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "search query"},
				"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "default": 5},
			},
			"required": []string{"query"},
		},
	}
}

// mcpToolsCall 执行 tools/call。参数与鉴权判据与 /v1/search 同源。
func (h *Handler) mcpToolsCall(obs *responseObserver, r *http.Request, req jsonRPCRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &jsonRPCError{Code: -32602, Message: "invalid params: " + err.Error()}})
			return
		}
	}
	if params.Name != mcpToolName {
		writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCError{Code: -32602, Message: "unknown tool: " + params.Name}})
		return
	}

	var args struct {
		Query      string `json:"query"`
		MaxResults *int   `json:"max_results"`
	}
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &jsonRPCError{Code: -32602, Message: "invalid arguments: " + err.Error()}})
			return
		}
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &jsonRPCError{Code: -32602, Message: "query is required and must be a non-empty string"}})
		return
	}
	maxResults := searchDefaultResults
	if args.MaxResults != nil {
		if *args.MaxResults < 1 || *args.MaxResults > searchMaxResults {
			writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &jsonRPCError{Code: -32602, Message: "max_results must be between 1 and 10"}})
			return
		}
		maxResults = *args.MaxResults
	}

	key := authKeyFrom(r.Context())
	pick, perr := pickForSearch(r, key)
	if perr != nil {
		var pe *searchPickError
		if errors.As(perr, &pe) {
			writeOpenAIError(obs, pe.Status, pe.Code, pe.Msg)
			return
		}
		writeOpenAIError(obs, http.StatusBadRequest, "invalid_request", perr.Error())
		return
	}

	if !h.searchLimiter.allow(pick.RateKey, time.Now()) {
		obs.Header().Set("Retry-After", "60")
		writeOpenAIError(obs, http.StatusTooManyRequests, "rate_limit_exceeded",
			"mcp rate limit exceeded (600 requests/minute per api key)")
		return
	}

	keyName := "main"
	if key != nil {
		keyName = key.Name
		if keyName == "" {
			keyName = "unnamed"
		}
	}

	results, _, lastErr := h.searchWithAccount(pick, keyName, query, maxResults)
	if results == nil {
		var uerr *upstream.Error
		if errors.As(lastErr, &uerr) {
			if uerr.Kind == upstream.ErrSoftRate {
				obs.Header().Set("Retry-After", "60")
				writeOpenAIError(obs, http.StatusTooManyRequests, "rate_limit_exceeded",
					"rate limited by upstream: "+uerr.Msg)
				return
			}
			writeOpenAIError(obs, http.StatusBadGateway, "upstream_error", uerr.Msg)
			return
		}
		if lastErr != nil {
			writeOpenAIError(obs, http.StatusBadGateway, "upstream_error", lastErr.Error())
			return
		}
		writeOpenAIError(obs, http.StatusServiceUnavailable, "no_healthy_account",
			"all accounts are temporarily unavailable, please retry later")
		return
	}

	payload, _ := json.Marshal(map[string]any{"object": "search_result", "results": results})
	writeJSON(obs, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: req.ID,
		Result: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(payload)}},
			"isError": false,
		}})
}
