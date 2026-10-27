package server

import "encoding/json"

// estimateAnthropicTokens 本地估算 Anthropic Messages 请求的 token 数。
// 口径：CJK 约 1 token/字，ASCII 约 4 字符/token，工具 JSON 按字节/4。
// 只用于 /v1/messages/count_tokens，不调用上游。
func estimateAnthropicTokens(req anthropicRequest) int {
	text := anthropicSystemText(req.System)
	for _, m := range req.Messages {
		text += anthropicContentText(m.Content)
	}
	ascii, cjk := 0, 0
	for _, r := range text {
		if r > 0x2E80 {
			cjk++
		} else {
			ascii++
		}
	}
	total := cjk + (ascii+3)/4
	for _, t := range req.Tools {
		b, _ := json.Marshal(t)
		total += (len(b) + 3) / 4
	}
	return total
}
