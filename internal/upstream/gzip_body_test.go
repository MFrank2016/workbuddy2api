package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestOutboundBodyGzipThreshold 小于阈值不压、超过阈值压且可解回原文。
func TestOutboundBodyGzipThreshold(t *testing.T) {
	c := &Client{GzipBody: true}
	small := bytes.Repeat([]byte("a"), gzipBodyThreshold-1)
	if out, enc := c.outboundBody(small); enc != "" || !bytes.Equal(out, small) {
		t.Fatalf("小于阈值不应压缩（enc=%q len=%d）", enc, len(out))
	}
	big := bytes.Repeat([]byte("上下文"), 30000) // 可压缩文本，>64KB
	out, enc := c.outboundBody(big)
	if enc != "gzip" {
		t.Fatalf("超过阈值应压缩，got enc=%q", enc)
	}
	if len(out) >= len(big) {
		t.Fatalf("压缩后应更小：%d >= %d", len(out), len(big))
	}
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	back, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(back, big) {
		t.Fatal("解压后与原文不一致")
	}
}

// TestOutboundBodyDisabled 关闭开关时永不压缩（排障路径）。
func TestOutboundBodyDisabled(t *testing.T) {
	c := &Client{GzipBody: false}
	big := bytes.Repeat([]byte("上下文"), 30000)
	if _, enc := c.outboundBody(big); enc != "" {
		t.Fatalf("关闭后不应压缩，got %q", enc)
	}
}

// TestGzipUnsupported 只认 415 与「解析失败类」400。
func TestGzipUnsupported(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusUnsupportedMediaType, "", true},
		{http.StatusBadRequest, `{"msg":"Unmarshal chat params failed"}`, true},
		{http.StatusBadRequest, `{"msg":"invalid character '\x1f' looking for beginning of value"}`, true},
		{http.StatusBadRequest, `{"msg":"content-encoding not supported"}`, true},
		{http.StatusBadRequest, `{"msg":"model is required"}`, false},
		{http.StatusTooManyRequests, `{"code":6004}`, false},
		{http.StatusInternalServerError, `oops`, false},
	}
	for _, c := range cases {
		if got := gzipUnsupported(c.status, c.body); got != c.want {
			t.Errorf("gzipUnsupported(%d, %q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestChannelBlockedReordersPaths 被渠道校验拦过的账号优先走 /console（issue #296）。
func TestChannelBlockedReordersPaths(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1"}
	if got := c.chatPaths(a); got[0] != chatCompletionsPath {
		t.Fatalf("默认应 /v2 优先，got %v", got)
	}
	c.markChannelBlocked("u1")
	got := c.chatPaths(a)
	if got[0] != chatConsolePath || got[1] != chatCompletionsPath {
		t.Fatalf("命中渠道校验后应 /console 优先，got %v", got)
	}
	// 未命中的其他账号不受影响
	if other := c.chatPaths(&auth.Auth{UID: "u2"}); other[0] != chatCompletionsPath {
		t.Fatalf("其他账号应保持 /v2 优先，got %v", other)
	}
}

// TestChannelBlockExpires TTL 到期后回落 /v2 主路径。
func TestChannelBlockExpires(t *testing.T) {
	c := &Client{}
	c.markChannelBlocked("u1")
	c.channelBlockedMu.Lock()
	c.channelBlocked["u1"] = time.Now().Add(-time.Minute) // 已过期
	c.channelBlockedMu.Unlock()
	if c.channelBlockedFor("u1") {
		t.Fatal("过期后不该再判为命中")
	}
	if got := c.chatPaths(&auth.Auth{UID: "u1"}); got[0] != chatCompletionsPath {
		t.Fatalf("过期后应回落 /v2，got %v", got)
	}
}

// TestIsChannelCheckBusinessCode issue #296 的现场报文：业务码是**字符串**
// "11-128"（带连字符），且不依赖 displayTips 的 channel verification 文案。
func TestIsChannelCheckBusinessCode(t *testing.T) {
	body := `{"code":"11-128","msg":"request illegal","requestId":"x"}`
	// 业务码分支必须自己成立：没有 channel verification 标记也能命中。
	if !isChannelRefused(body) {
		t.Fatal("code=\"11-128\" + request illegal 必须命中业务码分支")
	}
	if !IsChannelCheck(body) {
		t.Fatal("11-128 业务码必须识别为渠道校验")
	}
	// 本地语义：渠道校验是独立分类 ErrChannelRefused（不是 ErrAccountFault），
	// 这样 handler 不会把它当成 11140 账号级故障去罚号。
	if got := Classify(400, body); got != ErrChannelRefused {
		t.Fatalf("11-128 渠道校验应归 ErrChannelRefused，got %v", got)
	}
}

// TestChannelCheckResendsOnceOnBothPaths 两条路径都被渠道校验拦时，原样重发一次
// （issue #296：上游报文自己写着「请求被拦截，请重新发送」），仍失败才返回错误。
func TestChannelCheckResendsOnceOnBothPaths(t *testing.T) {
	channel := `{"code":11128,"msg":"request illegal","displayTips":{"en":"This channel verification failed"}}`
	var calls int
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(400, channel), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	_, _, _, err := c.ChatStream(a, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("两条路径 + 重发都被拦时应返回错误")
	}
	// /v2 + /console + 重发 1 次 = 3 次
	if calls != 3 {
		t.Fatalf("calls=%d want 3（/v2、/console、重发一次）", calls)
	}
}

// TestChannelBlockedKeepsGlobalSinglePath global 仍固定 /v2（#119 的 /console
// WAF 取舍）：路径记忆只调整 CN 域的候选顺序，不得把 global 改道 /console。
func TestChannelBlockedKeepsGlobalSinglePath(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"}
	c.markChannelBlocked("g1")
	got := c.chatPaths(a)
	if len(got) != 1 || got[0] != chatCompletionsPath {
		t.Fatalf("global 应保持单路径 /v2，got %v", got)
	}
}

// TestChatStreamGzipFallbackOn415 大请求体首发出 gzip；上游 415 时同路径改用
// 未压缩 body 重发，且 Content-Encoding 不残留。
func TestChatStreamGzipFallbackOn415(t *testing.T) {
	var bodies [][]byte
	var encs []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, b)
		encs = append(encs, r.Header.Get("Content-Encoding"))
		if len(bodies) == 1 {
			return jsonResp(http.StatusUnsupportedMediaType, `{"msg":"unsupported media type"}`), nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
		}, nil
	})
	c.GzipBody = true
	large := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("上下文", 30000) + `"}],"stream":true}`
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at"}, []byte(large), "", ChatMeta{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("ChatStream: status=%d err=%v", status, err)
	}
	if rc != nil {
		rc.Close()
	}
	if len(bodies) != 2 {
		t.Fatalf("calls=%d want 2（gzip 失败后未压缩重发）", len(bodies))
	}
	if encs[0] != "gzip" {
		t.Fatalf("首发应压缩，Content-Encoding=%q", encs[0])
	}
	if encs[1] != "" {
		t.Fatalf("重发不应残留 Content-Encoding，got %q", encs[1])
	}
	zr, err := gzip.NewReader(bytes.NewReader(bodies[0]))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(bodies[1], decoded) {
		t.Fatal("重发 body 应等于首发解压后的原文")
	}
}

// TestChatPathsNilAccount nil 账号按 CN 处理，且不得因 a.UID 解引用 panic。
func TestChatPathsNilAccount(t *testing.T) {
	c := &Client{}
	got := c.chatPaths(nil)
	if len(got) != 2 || got[0] != chatCompletionsPath || got[1] != chatConsolePath {
		t.Fatalf("nil 账号应回 CN 候选 [/v2, /console]，got %v", got)
	}
}
