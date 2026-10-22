// image_policy_test.go 图片计数与策略（upstream.max_images / image_policy）：
// 计数口径（字符串/对象、重复块、tool 结果）、truncate 保留最新 N 张、error 超限报错、
// 关闭态（max_images=0 / passthrough / 未知策略）逐字节不动。
package upstream

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// imagesBody 构造带 n 张图片的 chat 请求体（第 i 张 URL 带唯一标记 tag-i）。
func imagesBody(n int, tag string) []byte {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf(`{"type":"image_url","image_url":{"url":"https://example.test/%s-%d.png"}}`, tag, i))
	}
	return []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"look"},` +
		strings.Join(parts, ",") + `]}]}`)
}

// TestCountImages 计数口径正/负例：image_url 字符串形态与对象形态各计 1、
// 重复块分别计、tool 结果里的图片计入、非图片 part 不计、畸形 body 计 0。
func TestCountImages(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "字符串形态计 1",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: 1,
		},
		{
			name: "对象形态计 1",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://e.test/a.png"}}]}]}`,
			want: 1,
		},
		{
			name: "重复块分别计",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:a"},{"type":"image_url","image_url":"data:a"}]}]}`,
			want: 2,
		},
		{
			name: "tool 结果图片计入",
			body: `{"messages":[` +
				`{"role":"user","content":[{"type":"image_url","image_url":"data:a"}]},` +
				`{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:b"}},{"type":"text","text":"x"}]}]}`,
			want: 2,
		},
		{
			name: "跨消息合计",
			body: `{"messages":[` +
				`{"role":"user","content":[{"type":"image_url","image_url":"data:a"}]},` +
				`{"role":"assistant","content":"ok"},` +
				`{"role":"user","content":[{"type":"image_url","image_url":"data:b"},{"type":"image_url","image_url":"data:c"}]}]}`,
			want: 3,
		},
		{"纯文本 content 计 0", `{"messages":[{"role":"user","content":"hi"}]}`, 0},
		{"无 messages 计 0", `{"model":"m"}`, 0},
		{"畸形 JSON 计 0", `not json`, 0},
		{"空 body 计 0", ``, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CountImages([]byte(c.body)); got != c.want {
				t.Errorf("CountImages=%d want %d body=%s", got, c.want, c.body)
			}
		})
	}
}

// TestApplyImagePolicyTruncateKeepsNewest 17 张 + max_images=16 + truncate：
// 保留最新 16 张（丢弃最旧的 img-0，保留 img-16），其余字段与顺序不变。
func TestApplyImagePolicyTruncateKeepsNewest(t *testing.T) {
	src := imagesBody(17, "img")
	out, count, err := ApplyImagePolicy(src, 16, "truncate")
	if err != nil {
		t.Fatalf("ApplyImagePolicy err=%v", err)
	}
	if count != 17 {
		t.Errorf("count=%d want 17 (原始请求图片数)", count)
	}
	if got := CountImages(out); got != 16 {
		t.Fatalf("改写后图片数=%d want 16", got)
	}
	if bytes.Contains(out, []byte("img-0.png")) {
		t.Error("最旧的 img-0 必须被丢弃")
	}
	if !bytes.Contains(out, []byte("img-16.png")) {
		t.Error("最新的 img-16 必须保留")
	}
	obj, err := decodeBody(out)
	if err != nil {
		t.Fatalf("out not json: %v", err)
	}
	if obj["model"] != "glm-5.2" {
		t.Errorf("model=%v want glm-5.2（其余字段原样）", obj["model"])
	}
}

// TestApplyImagePolicyTruncateAcrossMessages 截断跨消息按出现顺序（最旧的先丢），
// tool 结果里的图片同样参与计数与截断。
func TestApplyImagePolicyTruncateAcrossMessages(t *testing.T) {
	src := []byte(`{"messages":[` +
		`{"role":"user","content":[{"type":"image_url","image_url":"data:old"}]},` +
		`{"role":"tool","content":[{"type":"image_url","image_url":"data:mid"}]},` +
		`{"role":"user","content":[{"type":"image_url","image_url":"data:new"}]}]}`)
	out, count, err := ApplyImagePolicy(src, 2, "truncate")
	if err != nil || count != 3 {
		t.Fatalf("err=%v count=%d want nil/3", err, count)
	}
	if bytes.Contains(out, []byte(`"data:old"`)) {
		t.Error("最旧的 user 图片必须被丢弃")
	}
	for _, keep := range []string{"data:mid", "data:new"} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Errorf("应保留 %s", keep)
		}
	}
}

// TestApplyImagePolicyError 超限 + error 策略 → *ImagePolicyError 带真实计数。
func TestApplyImagePolicyError(t *testing.T) {
	src := imagesBody(17, "img")
	out, count, err := ApplyImagePolicy(src, 16, "error")
	var limitErr *ImagePolicyError
	if !errors.As(err, &limitErr) {
		t.Fatalf("err=%v want *ImagePolicyError", err)
	}
	if limitErr.ImageCount != 17 || limitErr.MaxImages != 16 {
		t.Errorf("limitErr=%+v want {17 16}", limitErr)
	}
	if count != 17 {
		t.Errorf("count=%d want 17", count)
	}
	if !bytes.Equal(out, src) {
		t.Error("error 策略不得改写 body")
	}
}

// TestApplyImagePolicyUnderLimit error 策略未超限 → 原样返回（逐字节）。
func TestApplyImagePolicyUnderLimit(t *testing.T) {
	src := imagesBody(3, "img")
	out, count, err := ApplyImagePolicy(src, 16, "error")
	if err != nil || count != 3 {
		t.Fatalf("err=%v count=%d want nil/3", err, count)
	}
	if !bytes.Equal(out, src) {
		t.Error("未超限必须逐字节原样")
	}
}

// TestApplyImagePolicyNoop 关闭态（max_images=0 / passthrough / 空 / 未知策略）
// 一律逐字节原样——flag off 的 payload 等价性由本测试锁定。
func TestApplyImagePolicyNoop(t *testing.T) {
	src := imagesBody(17, "img")
	for _, tc := range []struct {
		name   string
		max    int
		policy string
	}{
		{"max_images=0 不限", 0, "truncate"},
		{"max_images 负数视为不限", -1, "error"},
		{"passthrough 不改写", 16, "passthrough"},
		{"空策略回落透传", 16, ""},
		{"未知策略不改写", 16, "bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, count, err := ApplyImagePolicy(src, tc.max, tc.policy)
			if err != nil {
				t.Fatalf("err=%v want nil（关闭态不得报错）", err)
			}
			if count != 0 {
				t.Errorf("count=%d want 0（关闭态不解析 body，0 = 未计数）", count)
			}
			if !bytes.Equal(out, src) {
				t.Error("关闭态必须逐字节原样")
			}
		})
	}
}

// TestApplyImagePolicyNoImages 无图片时任何开启态都不改字节（避免无谓 re-marshal）。
func TestApplyImagePolicyNoImages(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	for _, policy := range []string{"truncate", "error"} {
		out, count, err := ApplyImagePolicy(src, 1, policy)
		if err != nil || count != 0 {
			t.Fatalf("policy=%s err=%v count=%d want nil/0", policy, err, count)
		}
		if !bytes.Equal(out, src) {
			t.Errorf("policy=%s: 无图片必须逐字节原样", policy)
		}
	}
}
