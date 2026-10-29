package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DebugCapture 可选的完整请求诊断日志。默认关闭；开启后写 JSONL，按天分文件。
// 只记录元数据；FullRequests=true 时才由调用方附带完整 body。
type DebugCapture struct {
	Enabled      bool
	FullRequests bool
	Dir          string
}

var secretJSONKeys = map[string]bool{
	"authorization": true, "cookie": true, "set-cookie": true,
	"access_token": true, "refresh_token": true, "api_key": true, "admin_key": true,
	"x-api-key": true, "x-refresh-token": true,
}

var (
	bearerRe = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/-]+=*`)
	jwtRe    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	skRe     = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)
	pemRe    = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
)

// RedactJSON 递归脱敏：秘密键替换为 [REDACTED]，字符串中的 Bearer/JWT/sk-/PEM 替换。
func RedactJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if secretJSONKeys[strings.ToLower(k)] {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = RedactJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = RedactJSON(val)
		}
		return out
	case string:
		return redactString(x)
	default:
		return v
	}
}

func redactString(s string) string {
	s = bearerRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = jwtRe.ReplaceAllString(s, "[REDACTED_JWT]")
	s = skRe.ReplaceAllString(s, "[REDACTED_KEY]")
	s = pemRe.ReplaceAllString(s, "[REDACTED_PEM]")
	return s
}

// Capture 写一条脱敏 JSONL。未启用/目录不可写时静默丢弃，绝不阻塞模型请求。
func (d *DebugCapture) Capture(event string, payload map[string]any) {
	if d == nil || !d.Enabled {
		return
	}
	dir := d.Dir
	if dir == "" {
		dir = "./data"
	}
	rec := map[string]any{
		"ts":    time.Now().Format(time.RFC3339Nano),
		"event": event,
	}
	for k, v := range payload {
		rec[k] = RedactJSON(v)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	path := filepath.Join(dir, "debug-"+time.Now().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// captureInbound 记录入站请求元数据；FullRequests 时才附带完整 body。
func (h *Handler) captureInbound(r *http.Request, body []byte) {
	if h == nil || h.cfg.Debug == nil || !h.cfg.Debug.Enabled {
		return
	}
	payload := map[string]any{
		"path": r.URL.Path, "method": r.Method, "headers": r.Header,
	}
	if h.cfg.Debug.FullRequests {
		payload["body"] = json.RawMessage(body)
	}
	h.cfg.Debug.Capture("full_request_inbound", payload)
}
