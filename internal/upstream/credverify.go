package upstream

import (
	"context"
	"encoding/base64"
	"errors"
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/auth"
)

type jwtClaims struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
}

// parseJWTClaims 解析 JWT payload 的 sub/iss；非 JWT（无三段）返回错误。
func parseJWTClaims(token string) (*jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c jwtClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// validateRefreshedToken 在写回刷新结果前校验新 token 的身份：
// sub 必须匹配旧 UID；iss 不得跨 realm（CN <-> INTL）。
// 解析失败（opaque token）放行，避免因上游 token 形态变化阻断刷新。
func validateRefreshedToken(old *auth.Auth, newAccessToken string) error {
	if old == nil || strings.TrimSpace(old.UID) == "" {
		return nil
	}
	claims, err := parseJWTClaims(newAccessToken)
	if err != nil {
		return nil
	}
	if claims.Sub != "" && claims.Sub != old.UID {
		return fmt.Errorf("sub=%q does not match uid=%q", claims.Sub, old.UID)
	}
	iss := strings.ToLower(claims.Iss)
	if iss == "" {
		return nil
	}
	if old.Realm() == "global" && strings.Contains(iss, "copilot.tencent.com") {
		return fmt.Errorf("iss=%q looks CN but account realm is global", claims.Iss)
	}
	if old.Realm() == "cn" && strings.Contains(iss, "workbuddy.ai") {
		return fmt.Errorf("iss=%q looks INTL but account realm is cn", claims.Iss)
	}
	return nil
}

// CredentialStillUsable 在删除凭据前做一次只读可用性探测（get-user-resource）。
// 返回 keep=true 表示保留（可用或无法确定）；keep=false 表示上游明确判定授权失效。
// 403/5xx/网络/超时一律视为不确定 -> 保留，避免误删。
func (c *Client) CredentialStillUsable(ctx context.Context, a *auth.Auth) (keep bool, note string) {
	if a == nil {
		return true, "nil account"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := c.getUserResourceBody(a)
		done <- result{err: err}
	}()
	select {
	case <-ctx.Done():
		return true, "inconclusive: " + ctx.Err().Error()
	case r := <-done:
		if r.err == nil {
			return true, "still usable"
		}
		var ue *Error
		if errors.As(r.err, &ue) && ue.Status == 401 {
			return false, "unauthorized"
		}
		msg := strings.ToLower(r.err.Error())
		for _, marker := range []string{"invalid token", "token is not active", "session not found", "login expired"} {
			if strings.Contains(msg, marker) {
				return false, "unauthorized: " + marker
			}
		}
		return true, "inconclusive: " + r.err.Error()
	}
}
