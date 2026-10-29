package upstream

import (
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// PlanInfo 账号套餐识别结果（展示用，不参与选号）。
type PlanInfo struct {
	Label      string    `json:"label"`
	Paid       bool      `json:"paid"`
	Trial      bool      `json:"trial"`
	Enterprise bool      `json:"enterprise"`
	Stale      bool      `json:"stale"`
	CheckedAt  time.Time `json:"checked_at"`
	Source     string    `json:"source"`
}

// IdentifyPlan 从 get-user-resource 的套餐名/包码识别账号套餐档位。
// 企业号直接返回“企业版”；查询失败只标 stale，不影响额度/选号。
func (c *Client) IdentifyPlan(a *auth.Auth) (PlanInfo, error) {
	now := time.Now()
	if a == nil {
		return PlanInfo{Label: "待确认", Stale: true, CheckedAt: now}, nil
	}
	if a.EnterpriseID != "" {
		return PlanInfo{Label: "企业版", Enterprise: true, CheckedAt: now, Source: "enterprise"}, nil
	}
	resp, err := c.getUserResourceBody(a)
	if err != nil {
		return PlanInfo{Label: "待确认", Stale: true, CheckedAt: now}, err
	}
	best := PlanInfo{Label: "待确认", CheckedAt: now, Source: "package_name"}
	for _, p := range resp.Response.Data.Accounts {
		if !planActive(p.Status, p.StartTime, p.EndTime, now) {
			continue
		}
		label, paid, trial := planLabel(a.Realm(), p.PackageName, p.SubscriptionPackageCode)
		if label == "" {
			continue
		}
		best = PlanInfo{
			Label: label, Paid: paid, Trial: trial, CheckedAt: now,
			Source: planSource(p.SubscriptionPackageCode),
		}
		break
	}
	return best, nil
}

func planSource(code string) string {
	if strings.TrimSpace(code) != "" {
		return "package_code"
	}
	return "package_name"
}

// planActive 判断套餐是否在有效期内：Status 0/3 有效；1/2 无效；
// StartTime 在未来或 EndTime 已过视为无效；空时间不做限制。
func planActive(status int, start, end string, now time.Time) bool {
	if status == 1 || status == 2 {
		return false
	}
	if t, ok := parsePlanTime(start); ok && t.After(now) {
		return false
	}
	if t, ok := parsePlanTime(end); ok && !t.After(now) {
		return false
	}
	return true
}

func parsePlanTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation(packageEndLayout, s, softRateResetLoc); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// planLabel 把套餐名/包码映射成展示档位。CN 四档；INTL 免费/pro/Pro试用；
// 企业版优先；无法识别返回空串（调用方保留“待确认”）。
func planLabel(realm, name, code string) (label string, paid, trial bool) {
	text := strings.ToLower(strings.TrimSpace(name + " " + code))
	if text == "" {
		return "", false, false
	}
	if strings.Contains(text, "企业") || strings.Contains(text, "enterprise") {
		return "企业版", true, false
	}
	switch {
	case strings.Contains(text, "旗舰"):
		return "旗舰版", true, false
	case strings.Contains(text, "高级"):
		return "高级版", true, false
	case strings.Contains(text, "标准"):
		return "标准版", true, false
	case strings.Contains(text, "体验"):
		return "体验版", false, true
	}
	switch {
	case strings.Contains(text, "pro") && (strings.Contains(text, "试用") || strings.Contains(text, "trial")):
		return "Pro试用", true, true
	case strings.Contains(text, "pro"):
		return "pro", true, false
	case strings.Contains(text, "免费") || strings.Contains(text, "free"):
		return "免费", false, false
	}
	return "", false, false
}
