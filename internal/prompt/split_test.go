package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// SplitSystemMessages 是 11-128 的首选抢救：按句界拆成多条 system，文本一字不改、
// 承载仍在 system（指令优先级不变）。
func TestSplitSystemMessagesKeepsTextInSystem(t *testing.T) {
	const sys = "Tokens prefixed with @ are paths. Relative paths resolve from the workspace root. Anything else is a file."
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, ok := SplitSystemMessages(body)
	if !ok {
		t.Fatal("多句 system 应该能拆")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 3 条 system + 1 条 user, got %d: %v", len(msgs), msgs)
	}
	var joined []string
	for i := 0; i < 3; i++ {
		m := msgs[i].(map[string]any)
		if m["role"] != "system" {
			t.Errorf("第 %d 条角色应为 system: %v", i, m["role"])
		}
		joined = append(joined, m["content"].(string))
	}
	if strings.Join(joined, " ") != sys {
		t.Errorf("文本被改动:\n got=%q\nwant=%q", strings.Join(joined, " "), sys)
	}
	if msgs[3].(map[string]any)["content"] != "hi" {
		t.Error("user 消息被动了")
	}
}

// 单句 system 拆不动（切一刀反而破坏指令），返回 false 交给下一层。
func TestSplitSystemMessagesBailsOnSingleSentence(t *testing.T) {
	for _, sys := range []string{
		"You are a helpful assistant",             // 无句末标点
		"v1.2.3 is the current version",           // 句点后不是空白，不切
		"Use example.com for docs",                // 同上
	} {
		body, _ := json.Marshal(map[string]any{
			"messages": []any{map[string]any{"role": "system", "content": sys}},
		})
		if _, ok := SplitSystemMessages(body); ok {
			t.Errorf("single-sentence system must not split: %q", sys)
		}
	}
}

// MoveSystemToUser：system 原文搬进首条 user 前部，system 换成中性提示词。
func TestMoveSystemToUser(t *testing.T) {
	const sys = "You are a pirate. Always say ARRR."
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, ok := MoveSystemToUser(body, "You are a helpful assistant.")
	if !ok {
		t.Fatal("MoveSystemToUser should succeed")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "You are a helpful assistant." {
		t.Errorf("system not neutralised: %v", msgs[0])
	}
	user := msgs[1].(map[string]any)["content"].(string)
	if !strings.HasPrefix(user, sys) || !strings.HasSuffix(user, "hi") {
		t.Errorf("system text not moved into user: %q", user)
	}
}
