// request_images.go 请求体「是否带图」判定（capability 预检与 11133 image hint
// 共用同一事实来源）。
//
// 为什么单独成文件：handler.go 已承载整条 chat 管线，本判定是纯 body 遍历，
// 与调度/改写无关；同时它必须对**每一种** content 形态保持宽容，单独成文件
// 才便于把形态矩阵用测试钉死。
//
// 形态纪律（OpenAI 兼容客户端真实请求形状）：
//   - messages[].content 是字符串 → 纯文本，**本身永不携带图片**；但绝不能因此
//     让同一请求里其他消息的图片被漏判（单类型解码曾整包失败 → 预检 fail-open）。
//   - content 是数组 → 逐 part 检查：image_url（字符串或对象形态）、input_image、
//     以及 tool_result 这类自身再带 content 数组的 part（工具结果图片同样算图）。
//   - content 为 null / 缺失 / 其他标量 → 跳过该消息，继续扫其余消息。
//   - 单条畸形消息（非对象元素、非对象 part）只跳过自身，绝不污染整包判定。
package server

import "encoding/json"

// hasImagePart 报告聊天请求体是否携带多模态图片 part。body 非 JSON 对象 / 无
// messages / messages 非数组 → false（hint 侧宁缺勿滥：判不出带图就不给
// 「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return false
	}
	return messagesHaveImage(msgs)
}

// messagesHaveImage 遍历 messages，任一消息携带图片即 true（不短路解析：
// 解析已在入口一次完成，这里只是纯遍历）。
func messagesHaveImage(msgs []any) bool {
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue // 畸形消息项：跳过自身，继续扫
		}
		if contentHasImage(msg["content"]) {
			return true
		}
	}
	return false
}

// contentHasImage 判定单条消息的 content 是否携带图片。
// 字符串（及 null/其他标量）是纯文本或缺失 → false；数组逐 part 判定。
func contentHasImage(content any) bool {
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	return partsHaveImage(parts)
}

// partsHaveImage 遍历 content 数组；非对象 part（字符串/数字/null）跳过。
func partsHaveImage(parts []any) bool {
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if partHasImage(part) {
			return true
		}
	}
	return false
}

// partHasImage 判定单个 content part。image_url 的字符串与对象形态同属一个
// 语义单元（形状归一化在 upstream.normalizeImageURL，与判定解耦）；input_image
// 是 Responses 转换器的同义形态；tool_result 自身可再带 content 数组（工具
// 结果里的图片同样算图）。
func partHasImage(part map[string]any) bool {
	switch part["type"] {
	case "image_url", "input_image":
		return true
	case "tool_result":
		return contentHasImage(part["content"])
	}
	return false
}
