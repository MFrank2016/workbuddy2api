// images.go 文生图端点 POST /v1/images/generations（OpenAI 兼容）。
//
// 上游图像模型不进对话目录（按 tag text-to-image 剔除），所以模型名不走目录校验，
// 按 realm 前缀路由到对应账号池；请求只把 model 换成裸名后原样透传，回程把上游
// data 原样写出（已是 {created,data:[{url}],usage}，签名 COS 直链约 24h，不转存）。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/upstream"
)

// defaultImageModel 请求未带 model 时的缺省图像模型（国内版；realm 由前缀决定）。
const defaultImageModel = "cn:hunyuan-image-alpha"

// imageAttempts 失败换号上限（图像按次人工触发，3 次足够绕开坏号）。
const imageAttempts = 3

func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			`prompt is required（例：{"model":"`+defaultImageModel+`","prompt":"一只猫"}）`)
		return
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = defaultImageModel
	}
	realm, bare := resolveModel(model)
	// 与 chat 入口同款白/黑名单拒绝：不打上游、不占选号。
	if !h.allowedModel(realm, bare) {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
			"model is not in the gateway allowlist, try another model")
		return
	}
	if h.hiddenModel(realm, bare) {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway configuration, try another model")
		return
	}
	req["model"] = bare
	out, err := json.Marshal(req)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < imageAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, bare, realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		data, ierr := h.cfg.Upstream.ImageGenerations(a, json.RawMessage(out))
		if ierr == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		lastErr = ierr
		var ue *upstream.Error
		if errors.As(ierr, &ue) {
			// 请求级错误（参数/内容/模型权限）：换号必然同样被拒，fail-fast 保住池子。
			if requestLevelFailFastKind(ue.Kind) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				writeOpenAIError(w, status, "upstream_error", ue.Msg)
				return
			}
		}
	}
	if len(tried) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_account",
			"no available account for realm "+realm+"（该域没有可用的出图账号）")
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_error", lastErr.Error())
}
