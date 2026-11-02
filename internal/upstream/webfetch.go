// webfetch.go 上游网页抓取：POST {chat base}/agenttool/v1/webfetch。
// 返回上游原文 JSON（{url,title,content}，content 为 markdown），供 /v1/fetch 使用。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"workbuddy2api/internal/auth"
)

// webFetchPath 上游网页抓取端点（与 chat 同 base，realm 决定 CN / global）。
const webFetchPath = "/agenttool/v1/webfetch"

// WebFetch 抓取网页正文，返回上游原文 JSON。
func (c *Client) WebFetch(ctx context.Context, a *auth.Auth, url string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"url": url})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+webFetchPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.ChatHeaders(req, a, "", ChatMeta{})
	client, cerr := c.clientFor(a, false)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := doRequest(client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	// 200 也可以是业务错误（信封外确有 code 字段）：必须拦住，否则把错误当结果透传。
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return json.RawMessage(raw), nil
}
