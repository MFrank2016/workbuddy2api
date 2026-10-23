// profile_pick_test.go 产品 profile 作为选号第三维（Todo 22）。
//
// 钉住三件事：
//  1. profile 与 realm/groups 是 AND：只选同时满足三者的账号；
//  2. 全冷却兜底同样受 profile 约束——不能从别的 profile 借号（出站端点会错）；
//  3. 粘性命中路径也校验 profile（否则粘性会话绕过 profile 约束）。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func profilePool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "cn-cli", Domain: "copilot.tencent.com", Profile: "cn-cli", Groups: []string{"a"}})
	p.Add(&auth.Auth{UID: "cn-work", Domain: "www.workbuddy.cn", Profile: "cn-work", Groups: []string{"b"}})
	p.Add(&auth.Auth{UID: "intl-cli", Domain: "www.codebuddy.ai", Profile: "intl-cli"})
	p.Add(&auth.Auth{UID: "intl-work", Domain: "www.workbuddy.ai", Profile: "intl-work"})
	return p
}

func TestPickByProfile(t *testing.T) {
	p := profilePool(t)
	for _, tc := range []struct{ profile, realm, want string }{
		{"cn-cli", "cn", "cn-cli"},
		{"cn-work", "cn", "cn-work"},
		{"intl-cli", "global", "intl-cli"},
		{"intl-work", "global", "intl-work"},
	} {
		got := p.PickExcludingForRealmGroupsProfiles(nil, "", tc.realm, nil, tc.profile)
		if got == nil || got.UID != tc.want {
			t.Errorf("profile=%s realm=%s -> %v want %s", tc.profile, tc.realm, got, tc.want)
		}
	}
	// profile=="" 退化为现状：realm 内任一账号。
	if got := p.PickExcludingForRealmGroupsProfiles(nil, "", "cn", nil, ""); got == nil {
		t.Error("empty profile must not filter")
	}
	// profile 与 realm 同时生效：cn-work 账号不在 global 池里。
	if got := p.PickExcludingForRealmGroupsProfiles(nil, "", "global", nil, "cn-work"); got != nil {
		t.Errorf("cross-realm profile pick = %v want nil", got)
	}
}

func TestPickByProfileAndGroups(t *testing.T) {
	p := profilePool(t)
	if got := p.PickExcludingForRealmGroupsProfiles(nil, "", "cn", []string{"b"}, "cn-cli"); got != nil {
		t.Errorf("profile+groups mismatch = %v want nil", got)
	}
	if got := p.PickExcludingForRealmGroupsProfiles(nil, "", "cn", []string{"b"}, "cn-work"); got == nil || got.UID != "cn-work" {
		t.Errorf("profile+groups match = %v want cn-work", got)
	}
}

func TestStickyProfileMismatchReturnsNil(t *testing.T) {
	p := profilePool(t)
	if got := p.PickByUIDForModelGroupsProfiles("cn-cli", "", nil, "cn-cli"); got == nil {
		t.Error("matching profile sticky should hit")
	}
	if got := p.PickByUIDForModelGroupsProfiles("cn-cli", "", nil, "cn-work"); got != nil {
		t.Errorf("sticky bound to cn-cli must not satisfy cn-work, got %v", got)
	}
}

func TestFallbackNeverBorrowsAnotherProfile(t *testing.T) {
	p := profilePool(t)
	// 把所有 cn 账号冷却，只留 cn-cli 一个健康号。
	p.Cooldown("cn-work", CoolSoft, time.Hour, "test")
	p.Cooldown("intl-cli", CoolSoft, time.Hour, "test")
	p.Cooldown("intl-work", CoolSoft, time.Hour, "test")
	if got := p.PickExcludingForRealmGroupsProfiles(nil, "", "cn", nil, "cn-cli"); got == nil || got.UID != "cn-cli" {
		t.Errorf("healthy profile pick = %v want cn-cli", got)
	}
	// 再把 cn-cli 也冷却：同一 profile 的冷却号仍可兜底（这是既有语义）。
	p.Cooldown("cn-cli", CoolSoft, time.Hour, "test")
	if got := p.PickExcludingForRealmGroupsProfiles(nil, "", "cn", nil, "cn-cli"); got == nil {
		t.Error("same-profile cooling account should still be eligible for fallback")
	}
	// 但兜底不能跨 profile 借号：global 池里没有 cn-cli 账号，必须 nil。
	if got := p.PickExcludingForRealmGroupsProfiles(nil, "", "global", nil, "cn-cli"); got != nil {
		t.Errorf("fallback borrowed another profile: %v", got)
	}
}
