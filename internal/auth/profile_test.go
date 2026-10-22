package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// profileTestRealm 关掉逃生门影响：profile 推导不依赖 Realm()（它按原始字段判定），
// 但为了排除全局开关的干扰，这里统一显式打开。
func withGlobalOn(t *testing.T) {
	t.Helper()
	globalEnabled.Store(true)
	t.Cleanup(func() { globalEnabled.Store(true) })
}

// TestResolveProfilePrecedence 选择链：explicit > persisted derived > raw realm > domain > cn-cli。
//
// 这是契约 §1 的**唯一**优先级链，四个轴各写一条用例，并加两条「优先级打架」的
// 组合用例 —— 单轴都对、组合错是最容易漏的形态（例如把 realm 提到 derived 前面，
// 单看 realm 与 domain 的用例全绿）。
func TestResolveProfilePrecedence(t *testing.T) {
	withGlobalOn(t)
	cases := []struct {
		name              string
		explicit, derived string
		rawRealm, domain  string
		want              string
	}{
		{"全空 → cn-cli", "", "", "", "", "cn-cli"},
		{"explicit 最高", "intl-cli", "cn-work", "cn", "www.workbuddy.cn", "intl-cli"},
		{"derived 高于 realm", "", "cn-work", "cn", "copilot.tencent.com", "cn-work"},
		{"realm 高于 domain", "", "", "global", "www.codebuddy.ai", "intl-work"},
		{"domain 高于默认", "", "", "", "www.codebuddy.ai", "intl-cli"},
		{"空 realm 不当作 cn（否则 codebuddy.ai 会被误判）", "", "", "", "www.codebuddy.ai", "intl-cli"},
		{"未知 realm 忽略", "", "", "bogus", "www.workbuddy.cn", "cn-work"},
		{"显式空白忽略", "  ", "", "cn", "", "cn-cli"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveProfile(c.explicit, c.derived, c.rawRealm, c.domain); got != c.want {
				t.Errorf("ResolveProfile(%q,%q,%q,%q)=%q want %q",
					c.explicit, c.derived, c.rawRealm, c.domain, got, c.want)
			}
		})
	}
}

// TestResolveProfileDomainTable 4 个 domain-only 账号各自解析成对应 profile。
//
// 这 4 个 host 是契约表里唯一能确定 profile 的 domain；其余 host（如 codebuddy.cn）
// 被多个 profile 共用，不能猜 —— 落到默认 cn-cli。
func TestResolveProfileDomainTable(t *testing.T) {
	withGlobalOn(t)
	cases := []struct {
		domain, want string
	}{
		{"copilot.tencent.com", "cn-cli"},
		{"www.workbuddy.cn", "cn-work"},
		{"www.codebuddy.ai", "intl-cli"},
		{"www.workbuddy.ai", "intl-work"},
		// 归一化：大小写 / scheme / 末尾斜杠 / 端口
		{"https://WWW.WorkBuddy.CN/", "cn-work"},
		{"www.codebuddy.ai:443", "intl-cli"},
		// 共用 host 不猜
		{"www.codebuddy.cn", "cn-cli"},
		{"codebuddy.cn", "cn-cli"},
		{"", "cn-cli"},
	}
	for _, c := range cases {
		t.Run(c.domain, func(t *testing.T) {
			if got := ResolveProfile("", "", "", c.domain); got != c.want {
				t.Errorf("domain=%q profile=%q want %q", c.domain, got, c.want)
			}
		})
	}
}

// TestParseResolvesProfile 两种磁盘形态都要算出 profile（嵌套形 / 扁平形）。
func TestParseResolvesProfile(t *testing.T) {
	withGlobalOn(t)
	nested := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,` +
		`"domain":"www.workbuddy.cn"},"account":{"uid":"u1"}}`
	a, err := Parse([]byte(nested))
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if a.Profile != "cn-work" {
		t.Errorf("nested Profile=%q want cn-work", a.Profile)
	}
	if a.ProfileDerived != "" {
		t.Errorf("nested ProfileDerived=%q want empty", a.ProfileDerived)
	}

	flat := `{"accessToken":"at","uid":"u2","domain":"www.codebuddy.ai"}`
	b, err := Parse([]byte(flat))
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if b.Profile != "intl-cli" {
		t.Errorf("flat Profile=%q want intl-cli", b.Profile)
	}
}

// TestParseStoredRealmMapsProfile 旧文件（无 profile 字段）按**存储的** realm 映射：
// cn → cn-cli、global → intl-work，且 realm 优先于 domain（保证 flag-off 等价）。
func TestParseStoredRealmMapsProfile(t *testing.T) {
	withGlobalOn(t)
	cases := []struct {
		name, realm, domain, want string
	}{
		{"cn", "cn", "copilot.tencent.com", "cn-cli"},
		{"global 覆盖 domain", "global", "www.codebuddy.ai", "intl-work"},
		{"cn 覆盖 workbuddy.cn domain", "cn", "www.workbuddy.cn", "cn-cli"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"accessToken": "at", "uid": "u", "domain": c.domain, "realm": c.realm,
			})
			a, err := Parse(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Profile != c.want {
				t.Errorf("realm=%q domain=%q Profile=%q want %q", c.realm, c.domain, a.Profile, c.want)
			}
		})
	}
}

// TestParseExplicitProfileWins 显式 profile 压过 realm 与 domain。
func TestParseExplicitProfileWins(t *testing.T) {
	withGlobalOn(t)
	raw := `{"accessToken":"at","uid":"u","domain":"www.codebuddy.ai",` +
		`"realm":"global","profile":"intl-cli"}`
	a, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Profile != "intl-cli" {
		t.Errorf("Profile=%q want intl-cli", a.Profile)
	}
	// 显式值必须被记住，SaveAtomic 才能原样写回（不能把推导值写成显式）。
	if a.profileExplicit != "intl-cli" {
		t.Errorf("profileExplicit=%q want intl-cli", a.profileExplicit)
	}
}

// TestParseProfileInsideAuthSection 兼容写法：profile 写在 auth 段里也认（顶层优先）。
//
// 契约规定 profile 在顶层，但 realm 就在 auth 段里，手写时把 profile 一起塞进去
// 是很自然的写法。不认它会得到「配置写了 intl-cli、实际按 domain 推导」的
// 「看着对、行为不对」形态。顶层优先（两者都有时以顶层为准）。
func TestParseProfileInsideAuthSection(t *testing.T) {
	withGlobalOn(t)
	inner := `{"auth":{"accessToken":"at","uid":"u","domain":"www.codebuddy.ai",` +
		`"realm":"cn","profile":"intl-cli"},"account":{"uid":"u"}}`
	a, err := Parse([]byte(inner))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Profile != "intl-cli" {
		t.Errorf("auth.profile 未被识别：Profile=%q want intl-cli", a.Profile)
	}

	both := `{"auth":{"accessToken":"at","domain":"www.codebuddy.ai","profile":"cn-work"},` +
		`"account":{"uid":"u"},"profile":"intl-work"}`
	b, err := Parse([]byte(both))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if b.Profile != "intl-work" {
		t.Errorf("顶层 profile 必须优先：Profile=%q want intl-work", b.Profile)
	}
}

// TestParseDerivedOnlyWithBackfillFlag persisted derived 只在 profile_backfill_v1=true
// 时参与选择 —— 否则「面板手写的 profile_derived」会意外压过 realm。
func TestParseDerivedOnlyWithBackfillFlag(t *testing.T) {
	withGlobalOn(t)
	body := `{"accessToken":"at","uid":"u","realm":"cn","profile_derived":"intl-cli"`
	for _, c := range []struct {
		name string
		flag string
		want string
	}{
		{"flag=true → derived 生效", `,"profile_backfill_v1":true}`, "intl-cli"},
		{"flag 缺失 → 忽略 derived", `}`, "cn-cli"},
		{"flag=false → 忽略 derived", `,"profile_backfill_v1":false}`, "cn-cli"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := Parse([]byte(body + c.flag))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Profile != c.want {
				t.Errorf("Profile=%q want %q", a.Profile, c.want)
			}
		})
	}
}

// TestParseDoesNotWriteProfileBack 解析本身不得把推导结果落成显式 profile。
//
// 判据只能落在「下一次写盘」上：Parse 是纯内存函数，唯一能观察到回写的地方是
// SaveAtomic 的输出。若 SaveAtomic 无条件写 profile=a.Profile，那么
// www.codebuddy.ai（推导 intl-cli）被刷新一次 token 后就变成了显式 intl-cli，
// 之后管理员把 realm 改成 global 也不再变成 intl-work —— 语义被静默改写。
func TestParseDoesNotWriteProfileBack(t *testing.T) {
	withGlobalOn(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-domain.json")
	orig := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,` +
		`"domain":"www.codebuddy.ai"},"account":{"uid":"u1"}}`
	if err := os.WriteFile(fp, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse([]byte(orig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.FilePath = fp
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	doc := readDoc(t, fp)
	if _, has := doc["profile"]; has {
		t.Errorf("SaveAtomic 把推导值写成了显式 profile: %v", doc["profile"])
	}
}

// TestBackfillRealmPersistsDerivedProfile 关键回归：
// 无 realm 的 www.codebuddy.ai / www.workbuddy.cn 在
// Parse → BackfillRealm → SaveAtomic → 重新 Parse 后仍为 intl-cli / cn-work。
//
// 为什么必须走落盘往返：BackfillRealm 会写入 realm=cn（这两个 host 不属于
// .workbuddy.ai，现有 realm 规则判 cn）。若只把推导值留在内存，重启后按
// realm=cn 映射就会退化成 cn-cli —— 只有把推导值 + 版本标记落盘才能保住。
func TestBackfillRealmPersistsDerivedProfile(t *testing.T) {
	withGlobalOn(t)
	cases := []struct {
		domain, wantProfile, wantRealm string
	}{
		{"www.codebuddy.ai", "intl-cli", "cn"},
		{"www.workbuddy.cn", "cn-work", "cn"},
		{"copilot.tencent.com", "cn-cli", "cn"},
		{"www.workbuddy.ai", "intl-work", "global"},
	}
	for _, c := range cases {
		t.Run(c.domain, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "workbuddy-x.json")
			orig := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,` +
				`"domain":"` + c.domain + `"},"account":{"uid":"u1"}}`
			if err := os.WriteFile(fp, []byte(orig), 0o600); err != nil {
				t.Fatal(err)
			}
			a, err := Parse([]byte(orig))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			a.FilePath = fp
			changed, realm := a.BackfillRealm()
			if !changed {
				t.Fatalf("backfill changed=false want true")
			}
			if realm != c.wantRealm {
				t.Fatalf("backfill realm=%q want %q（Realm 规则不得改）", realm, c.wantRealm)
			}
			if a.ProfileDerived != c.wantProfile {
				t.Errorf("ProfileDerived=%q want %q", a.ProfileDerived, c.wantProfile)
			}
			if !a.profileBackfillV1 {
				t.Error("profile_backfill_v1 未置位")
			}
			if err := a.SaveAtomic(); err != nil {
				t.Fatalf("save: %v", err)
			}
			doc := readDoc(t, fp)
			if doc["profile_derived"] != c.wantProfile {
				t.Errorf("落盘 profile_derived=%v want %q", doc["profile_derived"], c.wantProfile)
			}
			if doc["profile_backfill_v1"] != true {
				t.Errorf("落盘 profile_backfill_v1=%v want true", doc["profile_backfill_v1"])
			}
			if _, has := doc["profile"]; has {
				t.Errorf("BackfillRealm 不该写显式 profile: %v", doc["profile"])
			}

			raw, err := os.ReadFile(fp)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Parse(raw)
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}
			if again.Profile != c.wantProfile {
				t.Errorf("重启后 Profile=%q want %q（stored realm=%q）",
					again.Profile, c.wantProfile, again.realm)
			}
		})
	}
}

// TestBackfillRealmKeepsExplicitProfile 有显式 profile 时，backfill 落的是推导值，
// 显式值原样保留（两者互不覆盖）。
func TestBackfillRealmKeepsExplicitProfile(t *testing.T) {
	withGlobalOn(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-explicit.json")
	orig := `{"accessToken":"at","uid":"u","domain":"www.codebuddy.ai","profile":"cn-work"}`
	if err := os.WriteFile(fp, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse([]byte(orig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.FilePath = fp
	if a.Profile != "cn-work" {
		t.Fatalf("Profile=%q want cn-work（显式优先）", a.Profile)
	}
	if changed, _ := a.BackfillRealm(); !changed {
		t.Fatal("backfill should persist realm")
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	doc := readDoc(t, fp)
	if doc["profile"] != "cn-work" {
		t.Errorf("显式 profile 未保留: %v", doc["profile"])
	}
	if doc["profile_derived"] != "intl-cli" {
		t.Errorf("profile_derived=%v want intl-cli（推导值）", doc["profile_derived"])
	}
	raw, _ := os.ReadFile(fp)
	again, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if again.Profile != "cn-work" {
		t.Errorf("重启后 Profile=%q want cn-work", again.Profile)
	}
}

// TestSaveAtomicProfileRoundTrip 显式 profile / 已持久化的 derived 都要能往返，
// 且刷新 token 不会把它们弄丢（SaveAtomic 是固定字段集整体重写）。
func TestSaveAtomicProfileRoundTrip(t *testing.T) {
	withGlobalOn(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-roundtrip.json")
	orig := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,` +
		`"domain":"www.workbuddy.cn","realm":"cn"},"account":{"uid":"u1"},` +
		`"profile":"intl-work","profile_derived":"cn-work","profile_backfill_v1":true}`
	if err := os.WriteFile(fp, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse([]byte(orig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.FilePath = fp
	a.AccessToken = "at2"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	doc := readDoc(t, fp)
	if doc["profile"] != "intl-work" {
		t.Errorf("profile=%v want intl-work", doc["profile"])
	}
	if doc["profile_derived"] != "cn-work" {
		t.Errorf("profile_derived=%v want cn-work", doc["profile_derived"])
	}
	if doc["profile_backfill_v1"] != true {
		t.Errorf("profile_backfill_v1=%v want true", doc["profile_backfill_v1"])
	}
	raw, _ := os.ReadFile(fp)
	again, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if again.Profile != "intl-work" || again.ProfileDerived != "cn-work" {
		t.Errorf("roundtrip Profile=%q Derived=%q want intl-work/cn-work",
			again.Profile, again.ProfileDerived)
	}
}

// TestRealmSemanticsUnchanged profile 的引入不得改变 Realm()（必须只读原字段）。
func TestRealmSemanticsUnchanged(t *testing.T) {
	withGlobalOn(t)
	cases := []struct {
		realm, domain, profile, want string
	}{
		{"", "www.codebuddy.ai", "intl-cli", "cn"},
		{"", "www.workbuddy.ai", "cn-cli", "global"},
		{"global", "www.codebuddy.cn", "cn-work", "global"},
	}
	for _, c := range cases {
		a := &Auth{realm: c.realm, Domain: c.domain, Profile: c.profile}
		if got := a.Realm(); got != c.want {
			t.Errorf("realm=%q domain=%q profile=%q Realm()=%q want %q（不得受 profile 影响）",
				c.realm, c.domain, c.profile, got, c.want)
		}
	}
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return doc
}
