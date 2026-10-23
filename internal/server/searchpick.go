// searchpick.go search/MCP 共用的请求解析与限流（契约 §4）。
//
// 与 search.go（HTTP 端点流程）分开：本文件的 pickForSearch 是 Todo 27 的 MCP
// tools/call **必须复用**的同一份判据（realm/groups/profile + 限流键），放在
// 端点文件里会让 MCP 反向依赖 search 的请求处理细节。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	// searchRateLimit 每上游 key 每分钟允许的请求数（契约 §4）。
	searchRateLimit = 600
	// searchRateWindow 固定窗口长度（契约 §4）。
	searchRateWindow = time.Minute
)

// searchPick 一次 search/MCP 请求解析出的选号维度与限流键。
//
// realm/groups 交给 pool 选号，profile 供 profile-aware 选号与出站路由，
// RateKey 是限流桶键（SHA-256(Authorization token)）。
type searchPick struct {
	Realm   string
	Groups  []string
	Profile string
	RateKey string
}

// searchPickError 解析期拒绝（realm/profile 冲突）。Status 恒 400；Code 是
// 对外错误码（key_realm_mismatch / profile_not_allowed / realm_required）。
type searchPickError struct {
	Status int
	Code   string
	Msg    string
}

func (e *searchPickError) Error() string { return e.Code + ": " + e.Msg }

// searchBadRequest 构造 400 类解析错误。
func searchBadRequest(code, msg string) *searchPickError {
	return &searchPickError{Status: http.StatusBadRequest, Code: code, Msg: msg}
}

// pickForSearch 解析一次 search/MCP 请求的 realm / groups / profile / 限流键。
//
// realm 推导**唯一链**（契约 §4）：
//
//	header profile > key.realm > key.profiles 隐含的单一 realm > cn
//
// 关键约束：
//   - X-WB2A-Realm 只用于**校验**（与 key.realm 不等 → 400 key_realm_mismatch），
//     不参与推导 —— 客户端头不能决定路由（否则等于给分组密钥开域绕过口）。
//   - 头缺失/空 = 回落 key.realm / profile 推导，**从不强制要求该头**。
//   - key.profiles 跨 realm 且既无 header profile 也无 key.realm → 400 realm_required：
//     猜一个 realm 会把请求打到错误域，报错比乱猜安全。
//   - 限流键恒为 SHA-256(Authorization token)：客户端自造的限流头/查询参数无效。
func pickForSearch(r *http.Request, key *AuthKey) (searchPick, error) {
	pick := searchPick{}
	var headerProfile, headerRealm string
	if r != nil {
		headerProfile = strings.TrimSpace(r.Header.Get("X-WB2A-Profile"))
		headerRealm = strings.TrimSpace(r.Header.Get("X-WB2A-Realm"))
		pick.RateKey = hashRateKey(r.Header.Get("Authorization"))
	}
	if key != nil && len(key.Groups) > 0 {
		pick.Groups = append([]string(nil), key.Groups...)
	}

	// profile 允许集：头值必须落在 key.profiles 内（空 = 不限，含 "*" = 任意）。
	if headerProfile != "" && key != nil && !profileAllowed(key.Profiles, headerProfile) {
		return pick, searchBadRequest("profile_not_allowed",
			"profile "+headerProfile+" is not allowed for this api key")
	}

	// realm 头与密钥绑定比对：只在 key 有 realm 且头非空时生效。
	if key != nil {
		if bound := strings.TrimSpace(key.Realm); bound != "" && headerRealm != "" && headerRealm != bound {
			return pick, searchBadRequest("key_realm_mismatch",
				"this api key is bound to realm "+bound+", but X-WB2A-Realm is "+headerRealm)
		}
	}

	// profile：header 优先；否则单一具体 profile 的 key 直接采用（["*"] 不贡献）。
	// 注意：这个回落 profile **不参与 realm 推导**——推导链里 key.realm 排在
	// key.profiles 之前（契约 §4），让 key.profiles 推出 realm 去压 key.realm 等于
	// 让配置自相矛盾的密钥绕过域绑定。
	profile := headerProfile
	if profile == "" && key != nil && len(key.Profiles) == 1 {
		if p := strings.TrimSpace(key.Profiles[0]); p != "" && p != "*" {
			profile = p
		}
	}
	pick.Profile = profile

	// realm 推导唯一链：header profile > key.realm > key.profiles 隐含单一 realm > cn。
	realm := ""
	if headerProfile != "" {
		realm = auth.ProfileRealm(headerProfile)
		// header profile 推出的 realm 与 key.realm 冲突：头不得绕过域绑定。
		if realm != "" && key != nil {
			if bound := strings.TrimSpace(key.Realm); bound != "" && bound != realm {
				return pick, searchBadRequest("key_realm_mismatch",
					"this api key is bound to realm "+bound+", but profile "+headerProfile+" routes to "+realm)
			}
		}
	}
	if realm == "" && key != nil {
		realm = strings.TrimSpace(key.Realm)
	}
	if realm == "" && key != nil {
		r, ok := singleRealmOfProfiles(key.Profiles)
		if !ok {
			return pick, searchBadRequest("realm_required",
				"X-WB2A-Profile is required: this api key allows profiles from more than one realm")
		}
		realm = r
	}
	if realm == "" {
		// 空 profiles / 无 key / 认不出的 profile → cn（契约 §4 链末端）。
		realm = "cn"
	}
	pick.Realm = realm
	return pick, nil
}

// profileAllowed 报告 want 是否在 allowed 内（空集 = 不限，"*" = 任意）。
func profileAllowed(allowed []string, want string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, p := range allowed {
		p = strings.TrimSpace(p)
		if p == "*" || strings.EqualFold(p, want) {
			return true
		}
	}
	return false
}

// singleRealmOfProfiles 返回 profiles 隐含的单一 realm；跨 realm 返回 ok=false。
// 认不出 realm 的条目（"*" / 未知值）不贡献约束。
func singleRealmOfProfiles(profiles []string) (string, bool) {
	realm := ""
	for _, p := range profiles {
		p = strings.TrimSpace(p)
		if p == "" || p == "*" {
			continue
		}
		r := auth.ProfileRealm(p)
		if r == "" {
			continue
		}
		if realm == "" {
			realm = r
			continue
		}
		if realm != r {
			return "", false
		}
	}
	return realm, true
}

// hashRateKey 把 Authorization token 归成限流桶键（SHA-256 hex）。
// 用哈希而非原文：限流表是进程内长生命周期结构，不该以明文保存密钥。
func hashRateKey(authz string) string {
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(authz), "Bearer "))
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// searchWindow 一个限流桶的固定窗口状态。
type searchWindow struct {
	start time.Time
	count int
}

// searchLimiter 进程内固定窗口限流（多实例各自计数，契约明确接受近似）。
type searchLimiter struct {
	mu      sync.Mutex
	windows map[string]*searchWindow
}

func newSearchLimiter() *searchLimiter {
	return &searchLimiter{windows: map[string]*searchWindow{}}
}

// allow 记录一次请求并报告是否仍在窗口配额内。窗口从桶的首次请求起算，
// 满 searchRateWindow 后重置（不做滑动窗口：契约要求固定窗口）。
func (l *searchLimiter) allow(key string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[key]
	if w == nil || !now.Before(w.start.Add(searchRateWindow)) {
		l.windows[key] = &searchWindow{start: now, count: 1}
		return true
	}
	if w.count >= searchRateLimit {
		return false
	}
	w.count++
	return true
}
