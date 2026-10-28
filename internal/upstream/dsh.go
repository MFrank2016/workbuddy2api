package upstream

import "strings"

const dshTextInterface = "You are a helpful assistant operating in a text interface."

// ApplyDSHCompatibility 归一化出站 Chat body 中 system/assistant/developer 文本里的
// DSH 身份句与 GUI 环境句。只处理字符串内容与标准 text block，跳过 fenced/
// indented/inline code；user、tool 及其他 block 不受影响。target 为空时使用
// WorkBuddy。重复调用结果不变。
func ApplyDSHCompatibility(body []byte, target string) []byte {
	target = strings.TrimSpace(target)
	if target == "" {
		target = "WorkBuddy"
	}
	return applyMessageTextTransform(body, func(text string) string {
		return replaceDSHText(text, target)
	})
}

func replaceDSHText(text, target string) string {
	if text == "" {
		return text
	}
	return transformProseLines(text, func(line string) string {
		return transformOutsideInlineCode(line, func(s string) string {
			return replaceDSHSegment(s, target)
		})
	})
}

func replaceDSHSegment(s, target string) string {
	s = strings.ReplaceAll(s,
		"You are operating in the DSH GUI environment with window controls.",
		dshTextInterface)
	s = strings.ReplaceAll(s,
		"You are operating in the DSH GUI environment",
		dshTextInterface)
	s = strings.ReplaceAll(s,
		"You are an AI agent powered by DeepSeek Harness",
		"You are "+target)
	s = strings.ReplaceAll(s, "DSH GUI environment", "text interface")
	return s
}
