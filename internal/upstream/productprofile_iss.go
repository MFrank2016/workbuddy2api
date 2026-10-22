// productprofile_iss.go 产品 profile 的 refresh auth base 与 JWT `iss` 交叉校验。
//
// iss 只做 **host 级**校验（契约 §1），不参与 ① profile 选择链：codebuddy.cn 同时
// 被 cn-cli 与 cn-work 共用，iss 不足以单独定 profile。冲突（iss 指向另一个 realm）
// 时以 iss 为准并记 warning；缺失/不可解析/不在表内时退回表。
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/url"
	"strings"

	"workbuddy2api/internal/auth"
)

// profileAuthBase 返回 refresh 使用的 auth base（表值 + JWT iss 交叉校验）。
//
// iss 只做 **host 级**校验，不参与 ① profile 选择：codebuddy.cn 同时被 cn-cli
// 与 cn-work 共用，iss 不足以单独定 profile。冲突（iss 指向另一个 realm）时以
// iss 为准并记 warning；缺失/不可解析/不在表内时退回表值。逃生门优先：纯 CN
// 部署下不接受 iss 指向的 intl host（否则会绕过 global.enabled=false）。
func (c *Client) profileAuthBase(a *auth.Auth) string {
	desc, ok := c.ProfileFor(a)
	if !ok {
		return ""
	}
	issHost := jwtIssHost(a.AccessTokenValue())
	if issHost == "" {
		return desc.AuthBase
	}
	base, issRealm, ok := authBaseForHost(issHost)
	if !ok {
		return desc.AuthBase
	}
	if issRealm == "global" && !c.GlobalEnabled {
		// 逃生门：iss 指向国际版也不得把纯 CN 部署带出去。
		log.Printf("WARN: [upstream] profile: JWT iss=%s ignored (global.enabled=false, acct=%s)", issHost, a.UID)
		return desc.AuthBase
	}
	profileRealm := "cn"
	if isIntlProfile(desc.ID) {
		profileRealm = "global"
	}
	if issRealm != profileRealm {
		log.Printf("WARN: [upstream] profile: JWT iss=%s conflicts with profile=%s (realm=%s); using iss auth base %s (acct=%s)",
			issHost, desc.ID, profileRealm, base, a.UID)
	}
	return base
}

// authBaseForHost 把 iss host 映射到表内 auth base（返回 base 与其 realm）。
//
// 只有「唯一对应一个 auth base」的 host 才映射：copilot.tencent.com → cn-cli、
// codebuddy.cn → cn-work、codebuddy.ai → intl-cli、workbuddy.ai → intl-work。
// 其余 host 一律不认（宁可不校验，也不猜错端点）。
func authBaseForHost(host string) (base, realm string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "copilot.tencent.com":
		return "https://copilot.tencent.com", "cn", true
	case "www.codebuddy.cn", "codebuddy.cn":
		return "https://www.codebuddy.cn", "cn", true
	case "www.codebuddy.ai", "codebuddy.ai":
		return "https://www.codebuddy.ai", "global", true
	case "www.workbuddy.ai", "workbuddy.ai":
		return "https://www.workbuddy.ai", "global", true
	}
	return "", "", false
}

// jwtIssHost 从 accessToken（JWT）的 payload 中取 iss 的 host。
// 任何一步不可解析（非 JWT / 非法 base64 / 非对象 / iss 非字符串）都返回空串。
func jwtIssHost(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 容忍带 padding 的变体。
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var payload struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return hostOf(payload.Iss)
}

// hostOf 把 iss（可能是 URL 或裸 host）归一成 host；空/非法返回空串。
func hostOf(iss string) string {
	iss = strings.TrimSpace(iss)
	if iss == "" {
		return ""
	}
	if !strings.Contains(iss, "://") {
		iss = "https://" + iss
	}
	u, err := url.Parse(iss)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
