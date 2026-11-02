// clientpick.go 从 /v3/config 的同一次响应里提取「官方客户端菜单会显示什么」。
//
// data.models 是平台全量目录，客户端菜单从来不是它的全量：菜单取 data.agents 里
// 默认 agent 的 models，再叠三条客户端规则（2026-10-29 两域真机复核）：
//  1. 只列默认 agent 的 models（tags 含 default → name==cli → 首个非空）；
//     上游没下发 agents 时返回 nil，调用方照旧全量列出，不臆造白名单。
//  2. ModelRateLimitCap.lines[].paidId 不单独成项（客户端把它并进 freeId 那一行）。
//  3. fast-model / balanced-model / deep-model 三个速度预设即使不在名单里也照常
//     出现；auto / auto-chat 是「自动模式」开关本身，不是菜单行。
//
// 顺序：不在 agent 名单里的预设按快速/均衡/极致前置，再接 agent 原序过滤结果。
// CN 预设不在名单里故被前置；INTL 的 fast/balanced 本就在名单内，结果是纯 agent
// 原序（首项 Auto）——写成「预设一律前置」会让 INTL 顺序错位。
package upstream

import (
	"encoding/json"
	"strings"
)

// v3ClientPick 默认 agent 的模型名单与付费变体表。
type v3ClientPick struct {
	agentModels []string
	paidIDs     map[string]bool
}

// parseV3ClientPick 解析 data.agents 与 productFeaturesConfig.ModelRateLimitCap。
// 无 agents / 解析失败返回 nil（调用方回落全量，不裁剪）。
func parseV3ClientPick(raw []byte) *v3ClientPick {
	var env struct {
		Data struct {
			Agents []struct {
				Name   string   `json:"name"`
				Tags   []string `json:"tags"`
				Models []string `json:"models"`
			} `json:"agents"`
			ProductFeaturesConfig struct {
				ModelRateLimitCap struct {
					Lines []struct {
						FreeID string `json:"freeId"`
						PaidID string `json:"paidId"`
					} `json:"lines"`
				} `json:"ModelRateLimitCap"`
			} `json:"productFeaturesConfig"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Data.Agents) == 0 {
		return nil
	}
	chosen := -1
	// 规则 1a：tags 含 default。
	for i, ag := range env.Data.Agents {
		for _, t := range ag.Tags {
			if strings.EqualFold(strings.TrimSpace(t), "default") && len(ag.Models) > 0 {
				chosen = i
				break
			}
		}
		if chosen >= 0 {
			break
		}
	}
	// 规则 1b：name == cli。
	if chosen < 0 {
		for i, ag := range env.Data.Agents {
			if strings.EqualFold(strings.TrimSpace(ag.Name), "cli") && len(ag.Models) > 0 {
				chosen = i
				break
			}
		}
	}
	// 规则 1c：首个非空。
	if chosen < 0 {
		for i, ag := range env.Data.Agents {
			if len(ag.Models) > 0 {
				chosen = i
				break
			}
		}
	}
	if chosen < 0 {
		return nil
	}
	pick := &v3ClientPick{agentModels: env.Data.Agents[chosen].Models, paidIDs: map[string]bool{}}
	for _, ln := range env.Data.ProductFeaturesConfig.ModelRateLimitCap.Lines {
		if id := strings.TrimSpace(ln.PaidID); id != "" {
			pick.paidIDs[id] = true
		}
	}
	return pick
}

// isAutoModeID auto / auto-chat 是客户端「自动模式」开关，不是菜单项。
func isAutoModeID(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "auto", "auto-chat":
		return true
	}
	return false
}

// clientSpeedPresets 三个速度预设（菜单顺序：快速/均衡/极致）。
var clientSpeedPresets = []string{"fast-model", "balanced-model", "deep-model"}

// ordered 返回按客户端菜单顺序排列的可见 id；nil = 无可见性口径（调用方全量）。
func (p *v3ClientPick) ordered() []string {
	if p == nil || len(p.agentModels) == 0 {
		return nil
	}
	inAgent := func(id string) bool {
		for _, m := range p.agentModels {
			if strings.EqualFold(m, id) {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(p.agentModels)+len(clientSpeedPresets))
	// 不在 agent 名单里的速度预设前置（CN 前置；INTL 本就在名单内→不前置）。
	for _, preset := range clientSpeedPresets {
		if !inAgent(preset) {
			out = append(out, preset)
			seen[preset] = true
		}
	}
	for _, id := range p.agentModels {
		if isAutoModeID(id) || p.paidIDs[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
