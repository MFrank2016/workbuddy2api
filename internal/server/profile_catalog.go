// profile_catalog.go 产品 profile 的请求解析与目录缓存（契约 §1 / Todo 22）。
//
// 两件事刻意放在一起：
//  1. `resolveRequestProfile`：把 X-WB2A-Profile / key.profiles 解析成本次请求
//     实际使用的 profile；越界或未知值在**选号前**拒绝（400 profile_not_allowed）。
//  2. `profileModelInfos` / `warmProfileModels`：profile 维度的模型目录快照。
//     不同 profile 的模型清单与能力可能不同（intl-cli vs intl-work），能力预检
//     必须按 profile 读，不能拿 realm 目录冒充。
//
// 目录只读缓存、不在热路径拉上游：冷缓存 fail-open（与 Todo 12 同纪律）。
// 填充是异步的（warmProfileModels），每个 profile 最多每小时一次。
package server

import (
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

const profileModelsTTL = time.Hour

type profileModelEntry struct {
	infos   []upstream.ModelInfo
	fetched time.Time
}

// profilesActive 报告产品 profile 路由是否真正生效（契约 §1：只有
// enabled=true 且 detect_only=false 才使用 profile）。
func (h *Handler) profilesActive() bool {
	return h != nil && h.cfg.Upstream != nil &&
		h.cfg.Upstream.ProfilesEnabled && !h.cfg.Upstream.ProfilesDetectOnly
}

// profileError 解析期拒绝（400 profile_not_allowed）。
type profileError struct {
	Code string
	Msg  string
}

func (e *profileError) Error() string { return e.Code + ": " + e.Msg }

// resolveRequestProfile 解析本次请求的 profile。
//
// 顺序：X-WB2A-Profile（必须在该密钥的允许集内，且必须是已知 profile）>
// 密钥只允许一个具体 profile 时采用它 > 空（不限 profile）。
//
// profiles 未激活（enabled=false 或 detect_only=true）时恒返回空：
// 此时 key.profiles / header 都**不得影响选号**（契约 §1）。
func (h *Handler) resolveRequestProfile(r *http.Request, key *AuthKey) (string, *profileError) {
	if !h.profilesActive() {
		return "", nil
	}
	header := ""
	if r != nil {
		header = strings.TrimSpace(r.Header.Get("X-WB2A-Profile"))
	}
	if header != "" {
		if auth.ProfileRealm(header) == "" {
			return "", &profileError{Code: "profile_not_allowed",
				Msg: "unknown profile " + header + " (want cn-cli / cn-work / intl-cli / intl-work)"}
		}
		if key != nil && !profileAllowed(key.Profiles, header) {
			return "", &profileError{Code: "profile_not_allowed",
				Msg: "profile " + header + " is not allowed for this api key"}
		}
		return header, nil
	}
	if key != nil {
		if p, ok := singleConcreteProfile(key.Profiles); ok {
			return p, nil
		}
	}
	return "", nil
}

// singleConcreteProfile 返回允许集里唯一的具体 profile；空集 / "*" / 多个值
// 都不贡献（调用方据此不做 profile 过滤）。
func singleConcreteProfile(profiles []string) (string, bool) {
	if len(profiles) != 1 {
		return "", false
	}
	p := strings.TrimSpace(profiles[0])
	if p == "" || p == "*" || auth.ProfileRealm(p) == "" {
		return "", false
	}
	return p, true
}

// profileModelInfos 返回某 profile 的目录快照（TTL 内）；冷/过期返回 nil。
func (h *Handler) profileModelInfos(profile string) []upstream.ModelInfo {
	if h == nil || profile == "" {
		return nil
	}
	h.profileModelsMu.Lock()
	defer h.profileModelsMu.Unlock()
	e, ok := h.profileModels[profile]
	if !ok || len(e.infos) == 0 || time.Since(e.fetched) >= profileModelsTTL {
		return nil
	}
	return e.infos
}

// storeProfileModelInfos 写入某 profile 的目录快照（测试与 warm 共用）。
func (h *Handler) storeProfileModelInfos(profile string, infos []upstream.ModelInfo) {
	if h == nil || profile == "" || len(infos) == 0 {
		return
	}
	h.profileModelsMu.Lock()
	defer h.profileModelsMu.Unlock()
	if h.profileModels == nil {
		h.profileModels = map[string]profileModelEntry{}
	}
	h.profileModels[profile] = profileModelEntry{infos: infos, fetched: time.Now()}
}

// warmProfileModels 异步填充某 profile 的目录快照。
//
// 选一个该 profile 的账号拉一次 FetchModels；失败静默（下一次请求再试）。
// 不在请求路径同步调用：能力预检是热路径，冷缓存 fail-open 即可，不能为了
// 一次 400 把每个新 profile 的第一个请求拖慢成上游探测。
func (h *Handler) warmProfileModels(profile string) {
	if !h.profilesActive() || profile == "" || len(h.profileModelInfos(profile)) > 0 {
		return
	}
	acct := h.cfg.Pool.PickExcludingForRealmGroupsProfiles(nil, "", "", nil, profile)
	if acct == nil {
		return
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		return
	}
	h.storeProfileModelInfos(profile, infos)
}
