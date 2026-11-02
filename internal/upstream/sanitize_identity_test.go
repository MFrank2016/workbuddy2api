package upstream

import (
	"strings"
	"testing"
)

// TestSanitizeDshPathsParagraph DSH 0.3.x 的 @ 路径整段说明必须被同义改写
// （上游按连续原文匹配，原样发出即 400 11-128）。
func TestSanitizeDshPathsParagraph(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced. A relative path is resolved against the workspace root; a trailing slash marks a directory. Everything else is a file: read it with the read tool. The @\"...\" form only quotes a path containing spaces."
	out := sanitizeText(in)
	if strings.Contains(out, "Tokens prefixed with @") {
		t.Fatalf("DSH @ path paragraph not neutralised: %q", out)
	}
	if !strings.Contains(out, "Paths beginning with @ refer to files") {
		t.Fatalf("expected neutral replacement, got %q", out)
	}
	// 段外的 @ 不受影响（邮箱 / 包名）。
	for _, s := range []string{"contact me at a@b.com", "npm install @scope/pkg"} {
		if got := sanitizeText(s); got != s {
			t.Errorf("unrelated @ changed: %q -> %q", s, got)
		}
	}
}

// TestSanitizeIdentitySentence 身份句骨架（You are … powered by …）整句中性化：
// 只换品牌词、保留骨架仍会被上游当渠道特征。
func TestSanitizeIdentitySentence(t *testing.T) {
	cases := []string{
		"You are an AI agent powered by DeepSeek.",
		"You are a coding agent powered by Claude.",
		"You are WorkBuddy Agent, built by Tencent.",
		"You are an assistant created by Example Corp.",
	}
	for _, in := range cases {
		out := sanitizeText(in)
		for _, verb := range sanitizeIdentityVerbs {
			if strings.Contains(strings.ToLower(out), verb) {
				t.Errorf("identity skeleton %q survived: %q", verb, out)
			}
		}
		if !strings.Contains(out, "helpful AI assistant") {
			t.Errorf("expected neutral sentence, got %q", out)
		}
	}
	// 误伤守卫：分词修饰宾语的句子不该被改（窗口有界）。
	guard := "You are free to use tools built by the community."
	if got := sanitizeText(guard); got != guard {
		t.Errorf("false positive: %q -> %q", guard, got)
	}
}
