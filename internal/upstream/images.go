// images.go 文生图：POST {chatBase}/v2/images/generations。
// 回程信封 data 已是 OpenAI 形状 {created,data:[{url}],usage}，网关只拆信封不转存。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"workbuddy2api/internal/auth"
)

// imagesPath 上游文生图端点（与 chat 同 base，realm 决定 CN / global）。
const imagesPath = "/v2/images/generations"

// ImageGenerations 文生图，返回上游 data 对象（OpenAI 形状，可直接透出）。
func (c *Client) ImageGenerations(a *auth.Auth, body json.RawMessage) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+imagesPath, bytes.NewReader(body))
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
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
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}
