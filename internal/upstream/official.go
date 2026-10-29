// official 出站兼容 profile：模拟官方 WorkBuddy 桌面客户端（CN）的请求形状。
//
// 只在 CN 站 POST /v2/chat/completions 生效；legacy（默认）路径零改动。
// 本文件只提供 body 默认值 / headers / gzip 助手，ChatStreamContext 的接入
// 见 Task 5。
package upstream

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

// RequestProfile 出站请求兼容 profile。
type RequestProfile string

const (
	// ProfileLegacy 默认：逐字透传（零回归）。
	ProfileLegacy RequestProfile = "legacy"
	// ProfileOfficial 模拟官方 WorkBuddy 桌面客户端出站形状。
	ProfileOfficial RequestProfile = "official"
)

// officialChatPath official profile 唯一生效的路径。
const officialChatPath = "/v2/chat/completions"

// officialModel deepseek-v4.1-flash 的默认参数只对该模型生效。
const officialModel = "deepseek-v4.1-flash"

// ApplyOfficialBody 为 official profile 补齐官方客户端默认 body 字段。
// 显式值优先：已存在的字段一律不覆盖；无法解析的 body 原样返回。
func ApplyOfficialBody(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	if _, has := obj["max_tokens"]; !has {
		if v, ok := obj["max_completion_tokens"]; ok {
			obj["max_tokens"] = v
			delete(obj, "max_completion_tokens")
		}
	}
	if model, _ := obj["model"].(string); model == officialModel {
		if _, has := obj["max_tokens"]; !has {
			obj["max_tokens"] = 128000
		}
		if _, has := obj["temperature"]; !has {
			if _, hasTopP := obj["top_p"]; !hasTopP {
				obj["temperature"] = 1
			}
		}
		if _, has := obj["verbosity"]; !has {
			obj["verbosity"] = "high"
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// legacyOfficialHeaders official profile 必须移除的旧网关/CLI 头。
var legacyOfficialHeaders = []string{
	"Origin",
	"Referer",
	"X-Client-ID",
	"X-Client-Version",
	"X-Refresh-Token",
	"X-No-Authorization",
	"X-No-User-Id",
	"X-No-Enterprise-Id",
	"X-No-Department-Info",
}

// officialHeaders official profile 的精确头组。
var officialHeaders = [][2]string{
	{"Accept", "application/json"},
	{"User-Agent", "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"},
	{"X-Requested-With", "XMLHttpRequest"},
	{"x-stainless-arch", "x64"},
	{"x-stainless-lang", "js"},
	{"x-stainless-os", "Windows"},
	{"x-stainless-package-version", "6.25.0"},
	{"x-stainless-retry-count", "0"},
	{"x-stainless-runtime", "node"},
	{"x-stainless-runtime-version", "v22.21.1"},
	{"X-IDE-Type", "WorkBuddy"},
	{"X-IDE-Name", "WorkBuddy"},
	{"X-IDE-Version", "5.8.3"},
	{"X-Private-Data", "true"},
	{"X-Codebuddy-Request", "1"},
	{"X-Agent-Intent", "craft"},
	{"X-Agent-Purpose", "conversation"},
	{"X-Agent-Type", "main"},
	{"X-Product", "SaaS"},
}

// applyOfficialHeaders 在 CN 站 POST /v2/chat/completions 上应用 official 头组。
// 非 CN 或非该路径直接 no-op（INTL 保持 legacy）。
func (c *Client) applyOfficialHeaders(req *http.Request, a *auth.Auth) {
	if req == nil || a == nil || a.Realm() != "cn" || c.intlEndpoint(a) {
		return
	}
	if req.Method != http.MethodPost || req.URL == nil || !strings.HasSuffix(req.URL.Path, officialChatPath) {
		return
	}
	for _, name := range legacyOfficialHeaders {
		req.Header.Del(name)
	}
	for _, kv := range officialHeaders {
		req.Header.Set(kv[0], kv[1])
	}
	messageID := strings.ReplaceAll(req.Header.Get("X-Conversation-Message-ID"), "-", "")
	if messageID != "" {
		req.Header.Set("X-Request-ID", messageID)
	} else {
		req.Header.Del("X-Request-ID")
	}
}

// gzipBytes 用标准库 gzip 压缩出站 body。
func gzipBytes(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
