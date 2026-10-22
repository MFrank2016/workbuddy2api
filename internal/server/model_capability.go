// model_capability.go 模型能力预检的目录读取与错误响应（Todo 12）。
//
// 为什么独立成文件：handler.go 已远超单文件可维护体量，而「按 realm 读目录能力」
// 与「413 too_many_images 的响应形状」都是**可独立推理**的小单元——它们只依赖
// 模型目录缓存与 HTTP 响应写出，不碰选号/轮转/流式转发。放在 chatCompletions
// 旁边会让准入策略淹没在 2000 行的请求生命周期里。
package server

import (
	"fmt"
	"net/http"

	"workbuddy2api/internal/upstream"
)

// modelCapability 模型目录对某模型的能力声明（预检与 hint 共用的读取结果）。
type modelCapability struct {
	InCatalog         bool // 目录是否收录该模型（未收录 → 不做任何能力判定）
	SupportsImages    bool // supports_images 声明值
	SupportsImagesSet bool // 上游是否显式下发该字段（tri-state：false = 未知）
}

// lookupModelCapability 按 realm 读模型目录的 supports_images 声明。
//
// realm-aware：CN 读包级动态目录缓存（/v1/models 的 CN 探测结果），global 读
// Client 的 global 探测快照。两者都**只读已有缓存、不触发上游探测**——预检在
// 请求热路径上，为一个 400 去拉目录会把错误请求放大成上游请求（与 WAF IP
// fail-fast 的「不放大请求量」同纪律）。缓存冷 → InCatalog=false → 预检放行。
//
// profile-aware 目录分支（Todo 22）**不在此处**：本阶段只有 CN/global 两域。
func (h *Handler) lookupModelCapability(realm, bareModel string) modelCapability {
	if bareModel == "" {
		return modelCapability{}
	}
	if realm == "global" {
		if h.cfg.Upstream == nil {
			return modelCapability{}
		}
		for _, mi := range h.cfg.Upstream.GlobalModelInfosSnapshot() {
			if mi.ID == bareModel {
				return capabilityOf(mi)
			}
		}
		return modelCapability{}
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			return capabilityOf(mi)
		}
	}
	return modelCapability{}
}

// capabilityOf 把 ModelInfo 的能力字段映射成判定视图（单一映射点）。
func capabilityOf(mi upstream.ModelInfo) modelCapability {
	return modelCapability{
		InCatalog:         true,
		SupportsImages:    mi.SupportsImages,
		SupportsImagesSet: mi.SupportsImagesSet,
	}
}

// writeTooManyImages 413 too_many_images 响应体：error 对象带 image_count /
// max_images / param="messages"（契约字段，客户端据此提示用户减少图片）。
// message 用可读英文，code 是机器判据（manager 侧错误映射按 code 匹配）。
func writeTooManyImages(w http.ResponseWriter, imageCount, maxImages int) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
		"error": map[string]any{
			"message": fmt.Sprintf("request contains %d images, which exceeds upstream.max_images=%d; reduce the number of images in messages",
				imageCount, maxImages),
			"type":        "invalid_request_error",
			"code":        "too_many_images",
			"param":       "messages",
			"image_count": imageCount,
			"max_images":  maxImages,
		},
	})
}
