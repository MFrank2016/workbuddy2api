package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func wildcardOpt(rule map[string]string) IdentityOptions {
	return IdentityOptions{Enabled: true, Target: "WorkBuddy", Replacements: rule}
}

// TestIdentityWildcardReplacesDynamicSegment 受限通配：前缀/后缀固定、中间动态段
// 按 `(.*)` 匹配（Grok 句式），替换后保留句尾标点与正文。
func TestIdentityWildcardReplacesDynamicSegment(t *testing.T) {
	opt := wildcardOpt(map[string]string{"You are(.*)released by xAI": "You are WorkBuddy"})
	in := "You are Gemini 2.5 Pro released by xAI. Keep tools."
	want := "You are WorkBuddy. Keep tools."
	if got := ReplaceIdentityText(in, opt); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// 幂等：改写结果不应再被同一规则命中。
	if again := ReplaceIdentityText(want, opt); again != want {
		t.Fatalf("not idempotent: %q -> %q", want, again)
	}
}

// TestIdentityWildcardRejectsInvalidRules 不满足约束的规则整条丢弃：前后缀为空、
// 多个通配、含换行——宁可漏改，也不要产生过宽匹配。
func TestIdentityWildcardRejectsInvalidRules(t *testing.T) {
	in := "You are Foo released by xAI"
	for _, key := range []string{
		"(.*)released by xAI",   // 前缀为空
		"You are(.*)",           // 后缀为空
		"You are(.*)x(.*)y",     // 多个通配
		"You are\n(.*)released", // 前缀含换行
	} {
		opt := wildcardOpt(map[string]string{key: "You are WorkBuddy"})
		if got := ReplaceIdentityText(in, opt); got != in {
			t.Errorf("invalid rule %q must be ignored, got %q", key, got)
		}
	}
}

// TestIdentityWildcardLengthCap 动态段上限 256 个 Unicode 字符。
func TestIdentityWildcardLengthCap(t *testing.T) {
	opt := wildcardOpt(map[string]string{"You are(.*)released by xAI": "You are WorkBuddy"})
	long := "You are " + strings.Repeat("a", 300) + " released by xAI"
	if got := ReplaceIdentityText(long, opt); got != long {
		t.Fatalf("over-long dynamic segment must not match: %q", got)
	}
	ok := "You are " + strings.Repeat("a", 200) + " released by xAI"
	if got := ReplaceIdentityText(ok, opt); !strings.Contains(got, "You are WorkBuddy") {
		t.Fatalf("within-cap segment should match: %q", got)
	}
}

// TestIdentityLiteralBeatsWildcard 同一位置字面规则优先于通配（精确 wins）。
func TestIdentityLiteralBeatsWildcard(t *testing.T) {
	opt := wildcardOpt(map[string]string{
		"Grok":                       "GrokModel",
		"You are(.*)released by xAI": "You are WorkBuddy",
	})
	if got := ReplaceIdentityText("Grok released by xAI", opt); !strings.HasPrefix(got, "GrokModel") {
		t.Fatalf("literal rule must win: %q", got)
	}
}

// TestApplyIdentityWildcardSkipsUserMessages 通配同样只作用于 system/assistant/
// developer；user 消息原样保留。
func TestApplyIdentityWildcardSkipsUserMessages(t *testing.T) {
	opt := wildcardOpt(map[string]string{"You are(.*)released by xAI": "You are WorkBuddy"})
	body := `{"messages":[
		{"role":"system","content":"You are Gemini released by xAI."},
		{"role":"user","content":"You are Gemini released by xAI."}]}`
	out := ApplyIdentity([]byte(body), opt)
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Messages[0].Content != "You are WorkBuddy." {
		t.Errorf("system not rewritten: %q", obj.Messages[0].Content)
	}
	if obj.Messages[1].Content != "You are Gemini released by xAI." {
		t.Errorf("user message must stay untouched: %q", obj.Messages[1].Content)
	}
}
