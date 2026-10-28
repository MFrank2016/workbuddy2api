// identity.go 出站身份替换：把请求里暴露的客户端/模型身份词（Claude Code、Codex 等）
// 替换为配置的目标身份，规避上游按逐字身份指纹触发的 11-128 拦截。
//
// 作用域严格限定：只处理 system / assistant / developer 消息的字符串内容与标准
// text block；user、tool、reasoning、图片、工具参数一律不碰。文本内部还要跳过
// fenced code、缩进代码行与 inline code，避免改写用户/模型引用的代码。
package upstream

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
)

// IdentityOptions 身份替换配置。Enabled=false 时所有入口原样返回（零回归）。
type IdentityOptions struct {
	Enabled      bool
	Target       string
	Replacements map[string]string
}

// identityReplacement 排序后的替换项，key 以 rune 切片保存便于整词边界判断。
type identityReplacement struct {
	key    []rune
	target string
}

// SetIdentity 保存身份替换配置快照。深拷贝 map，避免调用方后续修改污染在途请求。
func (c *Client) SetIdentity(opt IdentityOptions) {
	c.identity.Store(cloneIdentityOptions(opt))
}

// IdentitySnapshot 返回当前身份替换配置快照；未设置时返回零值。
func (c *Client) IdentitySnapshot() IdentityOptions {
	if p := c.identity.Load(); p != nil {
		return *cloneIdentityOptions(*p)
	}
	return IdentityOptions{}
}

func cloneIdentityOptions(opt IdentityOptions) *IdentityOptions {
	out := IdentityOptions{Enabled: opt.Enabled, Target: opt.Target}
	if opt.Replacements != nil {
		out.Replacements = make(map[string]string, len(opt.Replacements))
		for k, v := range opt.Replacements {
			out.Replacements[k] = v
		}
	}
	return &out
}

// ApplyIdentity 对出站 Chat body 的 system/assistant/developer 文本做身份替换。
// 只重写命中的 content，其余顶层字段、消息字段（role/tool_call_id/extra 等）与
// 非 text block 原样保留；未命中时返回原始 body 字节。
func ApplyIdentity(body []byte, opt IdentityOptions) []byte {
	if !opt.Enabled || len(opt.Replacements) == 0 {
		return body
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	rawMsgs, ok := root["messages"]
	if !ok {
		return body
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body
	}
	changed := false
	for _, msg := range msgs {
		roleRaw, ok := msg["role"]
		if !ok {
			continue
		}
		var role string
		if json.Unmarshal(roleRaw, &role) != nil {
			continue
		}
		if role != "system" && role != "assistant" && role != "developer" {
			continue
		}
		contentRaw, ok := msg["content"]
		if !ok {
			continue
		}
		if text, ok := decodeJSONString(contentRaw); ok {
			if next := ReplaceIdentityText(text, opt); next != text {
				msg["content"] = mustMarshal(next)
				changed = true
			}
			continue
		}
		if !isJSONArray(contentRaw) {
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(contentRaw, &parts) != nil {
			continue
		}
		partsChanged := false
		for _, part := range parts {
			typeRaw, ok := part["type"]
			if !ok {
				continue
			}
			var typ string
			if json.Unmarshal(typeRaw, &typ) != nil || typ != "text" {
				continue
			}
			textRaw, ok := part["text"]
			if !ok {
				continue
			}
			text, ok := decodeJSONString(textRaw)
			if !ok || text == "" {
				continue
			}
			if next := ReplaceIdentityText(text, opt); next != text {
				part["text"] = mustMarshal(next)
				partsChanged = true
			}
		}
		if partsChanged {
			msg["content"] = mustMarshal(parts)
			changed = true
		}
	}
	if !changed {
		return body
	}
	root["messages"] = mustMarshal(msgs)
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func decodeJSONString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(trimmed, &s) != nil {
		return "", false
	}
	return s, true
}

func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// ReplaceIdentityText 对单段文本做身份替换。规则：
//   - 跳过 fenced code block（```）、行首 4 空格/Tab 的缩进代码行、inline code（`...`）；
//   - 行首 `You are <term>` 按前缀规则改写为 `You are <target>`；
//   - 其余位置只做整词替换（前后非字母/数字），key 按长度降序优先；
//   - key == target 的项跳过，保证幂等。
func ReplaceIdentityText(text string, opt IdentityOptions) string {
	if !opt.Enabled || text == "" || len(opt.Replacements) == 0 {
		return text
	}
	reps := make([]identityReplacement, 0, len(opt.Replacements))
	for k, v := range opt.Replacements {
		if k == "" || k == v {
			continue
		}
		reps = append(reps, identityReplacement{key: []rune(k), target: v})
	}
	if len(reps) == 0 {
		return text
	}
	sort.SliceStable(reps, func(i, j int) bool {
		return len(reps[i].key) > len(reps[j].key)
	})
	lines := strings.Split(text, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if inFence {
			if strings.HasPrefix(trimmed, "```") {
				inFence = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			inFence = true
			continue
		}
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
			continue
		}
		lines[i] = replaceIdentityLine(line, reps, opt.Target)
	}
	return strings.Join(lines, "\n")
}

func replaceIdentityLine(line string, reps []identityReplacement, target string) string {
	line = replaceLeadingIdentity(line, reps, target)
	return replaceOutsideInlineCode(line, reps)
}

// replaceLeadingIdentity 只处理行首（允许前导空白）的 `You are <term>` 前缀。
func replaceLeadingIdentity(line string, reps []identityReplacement, target string) string {
	const prefix = "You are "
	trimmed := strings.TrimLeft(line, " \t")
	offset := len(line) - len(trimmed)
	if !strings.HasPrefix(trimmed, prefix) {
		return line
	}
	rest := []rune(trimmed[len(prefix):])
	for _, rep := range reps {
		if len(rest) < len(rep.key) || !runesEqual(rest[:len(rep.key)], rep.key) {
			continue
		}
		if len(rest) > len(rep.key) && isWordRune(rest[len(rep.key)]) {
			continue
		}
		return line[:offset] + prefix + target + string(rest[len(rep.key):])
	}
	return line
}

// replaceOutsideInlineCode 逐段处理非 inline code 区域；`...`（任意长度反引号串）
// 原样保留。未闭合的反引号按普通字符处理。
func replaceOutsideInlineCode(line string, reps []identityReplacement) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] == '`' {
			n := 1
			for i+n < len(line) && line[i+n] == '`' {
				n++
			}
			if close := findBacktickRun(line, i+n, n); close >= 0 {
				end := close + n
				b.WriteString(line[i:end])
				i = end
				continue
			}
			b.WriteString(line[i : i+n])
			i += n
			continue
		}
		j := strings.IndexByte(line[i:], '`')
		if j < 0 {
			b.WriteString(wholeWordReplace(line[i:], reps))
			break
		}
		if j == 0 {
			continue
		}
		b.WriteString(wholeWordReplace(line[i:i+j], reps))
		i += j
	}
	return b.String()
}

func findBacktickRun(s string, from, n int) int {
	if from > len(s) {
		return -1
	}
	needle := strings.Repeat("`", n)
	idx := strings.Index(s[from:], needle)
	if idx < 0 {
		return -1
	}
	return from + idx
}

func wholeWordReplace(s string, reps []identityReplacement) string {
	if s == "" || len(reps) == 0 {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); {
		matched := false
		for _, rep := range reps {
			n := len(rep.key)
			if n == 0 || i+n > len(runes) || !runesEqual(runes[i:i+n], rep.key) {
				continue
			}
			if i > 0 && isWordRune(runes[i-1]) {
				continue
			}
			if i+n < len(runes) && isWordRune(runes[i+n]) {
				continue
			}
			b.WriteString(rep.target)
			i += n
			matched = true
			break
		}
		if !matched {
			b.WriteRune(runes[i])
			i++
		}
	}
	return b.String()
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
