// image_policy.go 图片计数与超限处置（upstream.max_images / upstream.image_policy）。
//
// 为什么单独成文件：payload.go 已承载出站改写的整条管线（stream/tool_choice/
// image_url 形状/thinking/tool 配对/脱敏），本策略是**请求治理**而非协议改写——
// 它回答「这张图要不要发出去」，不改变任何字段的编码形状。混进管线会让
// 「payload 改写」与「准入策略」两件事互相掩盖。
//
// 纪律：
//   - 默认关：max_images=0 或 policy=passthrough（含空串/未知值）时**逐字节**
//     返回入站体，绝不 re-marshal（flag-off 必须与迁移前逐字一致）。
//   - 计数口径 = `content[]` 里 `type=="image_url"` 的 part 数：字符串形态与对象
//     形态各计 1、重复块分别计、tool 结果的 content 同样计入（tool 结果也是
//     content 数组，遍历不分 role）。
//   - truncate 保留**最新** N 张（对话尾部是当前轮，最旧的历史图片先丢）。
package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ImagePolicyError 图片数超限且策略为 error 时的哨兵错误（handler 映射 413
// too_many_images）。用具体类型而非字符串：调用方按字段取真实计数写响应体，
// 不解析错误文案。
type ImagePolicyError struct {
	ImageCount int // 请求实际携带的图片数
	MaxImages  int // 配置上限
}

func (e *ImagePolicyError) Error() string {
	return fmt.Sprintf("too many images: %d > max_images %d", e.ImageCount, e.MaxImages)
}

// CountImages 统计请求体里的图片数（口径见文件头）。body 非 JSON 对象/无 messages
// → 0（调用方已有 JSON 合法性校验，这里不二次报错）。
func CountImages(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return 0
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return 0
	}
	return countImagesInMessages(msgs)
}

// isImagePart 判定 content part 是否为图片块。只认 type：image_url 的字符串形态与
// 对象形态都是同一个语义单元（形状归一化由 normalizeImageURL 负责，与计数解耦）。
func isImagePart(part map[string]any) bool {
	return part["type"] == "image_url"
}

// ApplyImagePolicy 按 maxImages/policy 处置入站图片，返回（可能被改写的）body、
// 原始图片数、错误。
//
//   - maxImages <= 0（0 = 不限）或 policy 为 passthrough/空/未知 → 原样返回，
//     count 返回 0（**不解析 body**：关闭态是每个请求的常态，不能白付一次
//     Unmarshal；0 在这里表示「未计数」，不是「没有图片」）；
//   - 未超限 → 原样返回，count = 原始图片数；
//   - truncate → 保留最新 maxImages 张，丢弃更旧的（仅动 content 数组，其余字段原样）；
//   - error → 返回 *ImagePolicyError（body 原样，不改写）。
//
// 返回的 count 恒为**原始**图片数（截断后也报原始值，便于日志/响应说明）。
func ApplyImagePolicy(body []byte, maxImages int, policy string) ([]byte, int, error) {
	if len(body) == 0 {
		return body, 0, nil
	}
	policy = strings.ToLower(strings.TrimSpace(policy))
	if maxImages <= 0 || policy == "" || policy == "passthrough" {
		return body, 0, nil
	}
	if policy != "truncate" && policy != "error" {
		// 未知策略：配置层已拒绝非法值；裸用路径按透传处理（不猜意图）。
		return body, 0, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, 0, nil
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body, 0, nil
	}
	count := countImagesInMessages(msgs)
	if count == 0 || count <= maxImages {
		return body, count, nil
	}
	if policy == "error" {
		return body, count, &ImagePolicyError{ImageCount: count, MaxImages: maxImages}
	}
	out, err := truncateImages(obj, msgs, count-maxImages)
	if err != nil {
		// marshal 失败（理论上只可能来自调用方塞入的非 JSON 值；本函数只处理
		// Unmarshal 产物）→ 不改写，保持入站体。
		return body, count, nil
	}
	return out, count, nil
}

// countImagesInMessages 统计 messages 里的图片数（CountImages 与 truncate 共用，
// 单一计数口径）。
func countImagesInMessages(msgs []any) int {
	n := 0
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			if part, ok := rawPart.(map[string]any); ok && isImagePart(part) {
				n++
			}
		}
	}
	return n
}

// truncateImages 丢弃最旧的 drop 张图片（按 messages 顺序 = 对话顺序），保留其余
// content part 的原顺序与消息结构。某条消息的图片被全部丢弃后 content 保留为空
// 数组——不删消息（删消息会改变对话轮次与 tool 配对，代价远大于一个空 content）。
func truncateImages(obj map[string]any, msgs []any, drop int) ([]byte, error) {
	remaining := drop
	for _, rawMsg := range msgs {
		if remaining <= 0 {
			break
		}
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if remaining > 0 && ok && isImagePart(part) {
				remaining--
				continue
			}
			kept = append(kept, rawPart)
		}
		msg["content"] = kept
	}
	obj["messages"] = msgs
	return json.Marshal(obj)
}
