// productprofile_test.go 钉住契约 §1 的产品 profile 路由：
// ① profile 选择链与 ② host 选择轴（只有 chat host 可被 override），
// refresh 的 [AuthBase, LegacyChatBase] 候选序列，JWT iss 交叉校验，
// 以及 flag 关闭时的 golden 等价。
package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// profileAuthJSON 四个 profile 的典型 auth 文件（domain 列取自契约表）。
func profileAuthJSON(profile string) string {
	domain := map[string]string{
		"cn-cli":    "copilot.tencent.com",
		"cn-work":   "www.workbuddy.cn",
		"intl-cli":  "www.codebuddy.ai",
		"intl-work": "www.workbuddy.ai",
	}[profile]
	return `{"accessToken":"at","refreshToken":"rt","uid":"u-` + profile + `","domain":"` + domain + `"}`
}

func mustProfileAuth(t *testing.T, raw string) *auth.Auth {
	t.Helper()
	a, err := auth.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	return a
}

// profileCapture 记录一次客户端生命周期内所有出站请求的 URL 与关键头。
type profileCapture struct {
	urls   []string
	paths  []string
	origin []string
	ua     []string
}

func (p *profileCapture) hosts(t *testing.T) []string {
	t.Helper()
	out := make([]string, 0, len(p.urls))
	for _, raw := range p.urls {
		out = append(out, urlHost(t, raw))
	}
	return out
}

func urlHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u.Host
}

// newProfileCaptureClient 构造一个所有出站请求都被记录、且按路径返回合法信封的 client。
// mutate 用于注入 profile 开关与 override 配置。
func newProfileCaptureClient(mutate func(*Client)) (*Client, *profileCapture) {
	cap := &profileCapture{}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			cap.urls = append(cap.urls, r.URL.String())
			cap.paths = append(cap.paths, r.URL.Path)
			cap.origin = append(cap.origin, r.Header.Get("Origin"))
			cap.ua = append(cap.ua, r.Header.Get("User-Agent"))
			switch {
			case strings.HasSuffix(r.URL.Path, "/v2/chat/completions"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
				}, nil
			case strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh"):
				return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
			case strings.HasSuffix(r.URL.Path, "/enterprises/personal/models"):
				return jsonResp(200, `{"code":0,"msg":"ok","data":{"models":[{"id":"m1","maxInputTokens":1000,"maxOutputTokens":4096}],"agents":[{"name":"cli","models":["m1"]}]}}`), nil
			default:
				return jsonResp(200, `{"code":0,"msg":"ok","data":{"credit":1,"energy":0,"already_claimed":false}}`), nil
			}
		})},
		ProfilesEnabled: true,
		// config global.enabled 缺省 true（生产 main.go 注入）；零值 false 是
		// 逃生门，会让所有 intl-* 断言意外回落 cn。
		GlobalEnabled: true,
	}
	if mutate != nil {
		mutate(c)
	}
	return c, cap
}

func mustChat(t *testing.T, c *Client, a *auth.Auth) {
	t.Helper()
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
}

func mustBilling(t *testing.T, c *Client, a *auth.Auth) {
	t.Helper()
	if _, err := c.billingJSON(a, http.MethodPost, "/billing/meter/get-user-resource", nil); err != nil {
		t.Fatalf("billing: %v", err)
	}
}

func mustWeb(t *testing.T, c *Client, a *auth.Auth) {
	t.Helper()
	if _, _, err := c.ClaimReward(a, "task_code"); err != nil {
		t.Fatalf("web claim: %v", err)
	}
}

func mustRefresh(t *testing.T, c *Client, a *auth.Auth) {
	t.Helper()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

// TestProductProfileHostsWithoutOverrides 无 override 时，4 个 profile 的
// chat / billing / web / refresh 全部打到契约表主机。
func TestProductProfileHostsWithoutOverrides(t *testing.T) {
	cases := []struct {
		profile  string
		chat     string
		billing  string
		web      string
		authBase string
		ua       string
	}{
		{"cn-cli", "copilot.tencent.com", "www.codebuddy.cn", "www.workbuddy.cn", "copilot.tencent.com", "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"},
		{"cn-work", "www.workbuddy.cn", "www.codebuddy.cn", "www.workbuddy.cn", "www.codebuddy.cn", "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"},
		{"intl-cli", "www.codebuddy.ai", "www.codebuddy.ai", "www.codebuddy.ai", "www.codebuddy.ai", "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1"},
		{"intl-work", "www.workbuddy.ai", "www.workbuddy.ai", "www.workbuddy.ai", "www.workbuddy.ai", "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1"},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			a := mustProfileAuth(t, profileAuthJSON(tc.profile))
			c, cap := newProfileCaptureClient(nil)

			mustChat(t, c, a)
			// models 目录走 chat base（企业端点路径按 profile 的 realm 选择）。
			if _, err := c.fetchEnterpriseModels(a); err != nil {
				t.Fatalf("models probe: %v", err)
			}
			mustBilling(t, c, a)
			mustWeb(t, c, a)
			mustRefresh(t, c, a)

			hosts := cap.hosts(t)
			want := []string{tc.chat, tc.chat, tc.billing, tc.web, tc.authBase}
			if len(hosts) != len(want) {
				t.Fatalf("requests=%v want %v", hosts, want)
			}
			for i := range want {
				if hosts[i] != want[i] {
					t.Errorf("request[%d] host=%q want %q (all=%v)", i, hosts[i], want[i], hosts)
				}
			}
			if got := cap.ua[0]; got != tc.ua {
				t.Errorf("chat UA=%q want %q", got, tc.ua)
			}
			// models 目录路径族按 profile 的 realm 选（intl-* → /v2/enterprises/...）。
			wantModelsPath := "/console/enterprises/personal/models"
			if strings.HasPrefix(tc.profile, "intl-") {
				wantModelsPath = "/v2/enterprises/personal/models"
			}
			if got := cap.paths[1]; got != wantModelsPath {
				t.Errorf("models path=%q want %q", got, wantModelsPath)
			}
		})
	}
}

// TestProductProfileInactiveMatchesGolden enabled=false 或 detect_only=true 时，
// chat 的 URL/头集合必须与迁移前 golden 逐字一致（三个非激活组合）。
func TestProductProfileInactiveMatchesGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(goldenDir, goldenChatTargetFile))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	cases := []struct {
		name       string
		enabled    bool
		detectOnly bool
	}{
		{"enabled_false_detect_only_true", false, true},
		{"enabled_true_detect_only_true", true, true},
		{"enabled_false_detect_only_false", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{
				HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
					}, nil
				})},
				ChatBaseCN:         "https://chat.example",
				ProfilesEnabled:    tc.enabled,
				ProfilesDetectOnly: tc.detectOnly,
			}
			got := captureChatWith(t, c, goldenAuth())
			render := "url: " + got.url + "\n" + renderGoldenHeaders(normalizeGoldenHeaders(got.head))
			if render != string(want) {
				t.Errorf("非激活组合改动了出站字节\n--- want ---\n%s\n--- got ---\n%s", want, render)
			}
		})
	}
}

// captureChatWith 与 captureGoldenChat 同构，但允许注入带 flag 的 Client。
func captureChatWith(t *testing.T, c *Client, a *auth.Auth) goldenChatTarget {
	t.Helper()
	var got goldenChatTarget
	c.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		got.url = r.URL.String()
		got.head = r.Header.Clone()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read outbound body: %v", err)
		}
		got.body = raw
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	rc, status, _, err := c.ChatStream(a, []byte(goldenChatInbound), "", goldenMeta())
	if err != nil || status != 200 {
		t.Fatalf("ChatStream: status=%d err=%v", status, err)
	}
	rc.Close()
	return got
}

// TestProductProfileInactiveKeepsLegacyBases flag 非激活时 billing/web/UA 仍走旧判据。
func TestProductProfileInactiveKeepsLegacyBases(t *testing.T) {
	cn := mustProfileAuth(t, profileAuthJSON("cn-cli"))
	global := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"g","domain":"www.workbuddy.ai"}`)
	for _, tc := range []struct {
		name       string
		enabled    bool
		detectOnly bool
	}{
		{"off", false, true},
		{"detect_only", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newProfileCaptureClient(func(c *Client) {
				c.ProfilesEnabled = tc.enabled
				c.ProfilesDetectOnly = tc.detectOnly
				c.BillingBaseCN = "https://billing.example"
			})
			if got := c.billingBase(cn); got != "https://billing.example" {
				t.Errorf("cn billing=%q want legacy override", got)
			}
			if got := c.webBase(global); got != defaultGlobalBase {
				t.Errorf("global web=%q want %q", got, defaultGlobalBase)
			}
			if got := c.userAgent(cn); strings.Contains(got, "WorkBuddy AI") {
				t.Errorf("cn UA=%q must stay CN legacy", got)
			}
		})
	}
}

// TestProductProfileInactiveIgnoresStoredProfile flag 非激活时，即使 auth 写了
// profile=intl-cli，路由仍按旧 realm 判据（stored realm=cn → CN base + CN UA）。
func TestProductProfileInactiveIgnoresStoredProfile(t *testing.T) {
	a := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"x","realm":"cn","domain":"www.codebuddy.ai","profile":"intl-cli"}`)
	for _, tc := range []struct {
		name       string
		enabled    bool
		detectOnly bool
	}{
		{"off", false, true},
		{"detect_only", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newProfileCaptureClient(func(c *Client) {
				c.ProfilesEnabled = tc.enabled
				c.ProfilesDetectOnly = tc.detectOnly
				c.ChatBaseCN = "https://chat.example"
				c.BillingBaseCN = "https://billing.example"
			})
			if got := c.chatBase(a); got != "https://chat.example" {
				t.Errorf("chat=%q want legacy ChatBaseCN", got)
			}
			if got := c.billingBase(a); got != "https://billing.example" {
				t.Errorf("billing=%q want legacy BillingBaseCN", got)
			}
			if got := c.webBase(a); got != "https://www.workbuddy.cn" {
				t.Errorf("web=%q want legacy cn default", got)
			}
			if got := c.userAgent(a); strings.Contains(got, "WorkBuddy AI") {
				t.Errorf("UA=%q must stay CN legacy (stored realm=cn)", got)
			}
			if _, ok := c.ProfileFor(a); ok {
				t.Error("ProfileFor must report inactive")
			}
		})
	}
}

// TestCheckinCallSetUnchangedWhenProfilesInactive flag 非激活时，签到这条排程
// 路径的**调用集**（请求数 + 目标 host/path）必须与 golden 一致——不是只看 chat URL。
func TestCheckinCallSetUnchangedWhenProfilesInactive(t *testing.T) {
	a := mustProfileAuth(t, profileAuthJSON("cn-work"))
	var baseline []string
	for _, tc := range []struct {
		name       string
		enabled    bool
		detectOnly bool
	}{
		{"off", false, false},
		{"enabled_false", false, true},
		{"detect_only", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cap := newProfileCaptureClient(func(c *Client) {
				c.ProfilesEnabled = tc.enabled
				c.ProfilesDetectOnly = tc.detectOnly
			})
			if err := c.DailyCheckin(a); err != nil {
				t.Fatalf("DailyCheckin: %v", err)
			}
			if len(cap.urls) == 0 {
				t.Fatal("checkin made no upstream call")
			}
			got := append([]string(nil), cap.urls...)
			if baseline == nil {
				baseline = got
				return
			}
			if strings.Join(got, "\n") != strings.Join(baseline, "\n") {
				t.Errorf("checkin call set changed under flag combo %s\nwant %v\ngot  %v",
					tc.name, baseline, got)
			}
		})
	}
}

// TestProductProfileOverrides 两个独立轴：override 只作用于对应 profile 类与对应 host。
func TestProductProfileOverrides(t *testing.T) {
	cnCLI := mustProfileAuth(t, profileAuthJSON("cn-cli"))
	cnWork := mustProfileAuth(t, profileAuthJSON("cn-work"))
	intlCLI := mustProfileAuth(t, profileAuthJSON("intl-cli"))
	intlWork := mustProfileAuth(t, profileAuthJSON("intl-work"))

	t.Run("global_billing_base_only_intl_billing", func(t *testing.T) {
		c, _ := newProfileCaptureClient(func(c *Client) { c.BillingBaseGlobal = "https://billing.override" })
		for _, a := range []*auth.Auth{intlCLI, intlWork} {
			if got := c.billingBase(a); got != "https://billing.override" {
				t.Errorf("%s billing=%q want override", a.Profile, got)
			}
		}
		// chat/auth 不受 billing override 影响。
		if got := c.chatBase(intlCLI); got != "https://www.codebuddy.ai" {
			t.Errorf("intl-cli chat=%q want table", got)
		}
		if got := c.profileAuthBase(intlCLI); got != "https://www.codebuddy.ai" {
			t.Errorf("intl-cli auth=%q want table", got)
		}
		// cn-* billing 无 override 键。
		for _, a := range []*auth.Auth{cnCLI, cnWork} {
			if got := c.billingBase(a); got != "https://www.codebuddy.cn" {
				t.Errorf("%s billing=%q want table (no cn override)", a.Profile, got)
			}
		}
	})

	t.Run("global_chat_base_only_intl_chat", func(t *testing.T) {
		c, _ := newProfileCaptureClient(func(c *Client) { c.ChatBaseGlobal = "https://chat.override" })
		for _, a := range []*auth.Auth{intlCLI, intlWork} {
			if got := c.chatBase(a); got != "https://chat.override" {
				t.Errorf("%s chat=%q want override", a.Profile, got)
			}
			if got := c.billingBase(a); got != tableFor(a.Profile).BillingBase {
				t.Errorf("%s billing=%q must stay table", a.Profile, got)
			}
			if got := c.profileAuthBase(a); got != tableFor(a.Profile).AuthBase {
				t.Errorf("%s auth=%q must stay table", a.Profile, got)
			}
		}
		for _, a := range []*auth.Auth{cnCLI, cnWork} {
			if got := c.chatBase(a); got != tableFor(a.Profile).ChatBase {
				t.Errorf("%s chat=%q must NOT take global override", a.Profile, got)
			}
		}
	})

	t.Run("cn_chat_base_only_cn_chat", func(t *testing.T) {
		c, _ := newProfileCaptureClient(func(c *Client) { c.ChatBaseCN = "https://cn.override" })
		for _, a := range []*auth.Auth{cnCLI, cnWork} {
			if got := c.chatBase(a); got != "https://cn.override" {
				t.Errorf("%s chat=%q want override", a.Profile, got)
			}
			if got := c.billingBase(a); got != "https://www.codebuddy.cn" {
				t.Errorf("%s billing=%q must stay table", a.Profile, got)
			}
			if got := c.profileAuthBase(a); got != tableFor(a.Profile).AuthBase {
				t.Errorf("%s auth=%q must stay table", a.Profile, got)
			}
		}
		for _, a := range []*auth.Auth{intlCLI, intlWork} {
			if got := c.chatBase(a); got != tableFor(a.Profile).ChatBase {
				t.Errorf("%s chat=%q must NOT take cn override", a.Profile, got)
			}
		}
	})

	t.Run("explicit_profile_with_override", func(t *testing.T) {
		a := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"x","realm":"cn","profile":"intl-cli"}`)
		c, _ := newProfileCaptureClient(func(c *Client) { c.ChatBaseGlobal = "https://chat.override" })
		if got := c.chatBase(a); got != "https://chat.override" {
			t.Errorf("explicit intl-cli chat=%q want override", got)
		}
	})
}

// TestProductProfileRefreshFallback refresh 候选序列 [AuthBase, LegacyChatBase]：
// 首选 4xx/网络错误时回落；5xx 不回落；cn-work 永远不打 www.workbuddy.cn。
func TestProductProfileRefreshFallback(t *testing.T) {
	t.Run("auth_base_404_falls_back_to_legacy", func(t *testing.T) {
		a := mustProfileAuth(t, profileAuthJSON("cn-work"))
		var urls []string
		c := &Client{
			ProfilesEnabled: true,
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				urls = append(urls, r.URL.String())
				if strings.Contains(r.URL.Host, "codebuddy.cn") {
					return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
				}
				return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
			})},
		}
		if err := c.RefreshToken(a); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if len(urls) != 2 {
			t.Fatalf("attempts=%v want 2", urls)
		}
		if !strings.HasPrefix(urls[0], "https://www.codebuddy.cn/v2/plugin/auth/token/refresh") {
			t.Errorf("first=%q want auth base codebuddy.cn", urls[0])
		}
		if !strings.HasPrefix(urls[1], "https://copilot.tencent.com/v2/plugin/auth/token/refresh") {
			t.Errorf("second=%q want legacy copilot.tencent.com", urls[1])
		}
		for _, u := range urls {
			if strings.Contains(u, "www.workbuddy.cn") {
				t.Errorf("cn-work refresh hit %q — 禁止（契约 QA failure）", u)
			}
		}
	})

	t.Run("network_error_falls_back", func(t *testing.T) {
		a := mustProfileAuth(t, profileAuthJSON("cn-work"))
		var urls []string
		c := &Client{
			ProfilesEnabled: true,
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				urls = append(urls, r.URL.String())
				if strings.Contains(r.URL.Host, "codebuddy.cn") {
					return nil, errors.New("connection refused")
				}
				return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
			})},
		}
		if err := c.RefreshToken(a); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if len(urls) != 2 || !strings.Contains(urls[1], "copilot.tencent.com") {
			t.Fatalf("attempts=%v want [auth, legacy]", urls)
		}
	})

	t.Run("server_error_does_not_fall_back", func(t *testing.T) {
		a := mustProfileAuth(t, profileAuthJSON("cn-work"))
		var urls []string
		c := &Client{
			ProfilesEnabled: true,
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				urls = append(urls, r.URL.String())
				return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
			})},
		}
		if err := c.RefreshToken(a); err == nil {
			t.Fatal("want error")
		}
		if len(urls) != 1 {
			t.Fatalf("attempts=%v want 1 (5xx 不回落)", urls)
		}
	})

	// LegacyChatBase 是**realm** 的旧默认 host（cn→copilot.tencent.com、
	// global→www.workbuddy.ai），不是 profile 的 chat base：intl-cli 账号的
	// stored realm 是 cn（codebuddy.ai 不是 .workbuddy.ai），故回落 copilot。
	t.Run("intl_cli_auth_base_then_legacy_realm_host", func(t *testing.T) {
		a := mustProfileAuth(t, profileAuthJSON("intl-cli"))
		var urls []string
		c := &Client{
			ProfilesEnabled: true,
			GlobalEnabled:   true,
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				urls = append(urls, r.URL.String())
				if strings.Contains(r.URL.Host, "codebuddy.ai") {
					return jsonResp(403, `{"code":403,"msg":"forbidden"}`), nil
				}
				return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
			})},
		}
		if err := c.RefreshToken(a); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if len(urls) != 2 {
			t.Fatalf("attempts=%v want 2", urls)
		}
		if !strings.Contains(urls[0], "www.codebuddy.ai") || !strings.Contains(urls[1], "copilot.tencent.com") {
			t.Errorf("attempts=%v want [codebuddy.ai, copilot.tencent.com]", urls)
		}
	})

	// LegacyChatBase 恒取 realm 旧默认 host，不受 global.chat_base 覆盖影响。
	t.Run("legacy_global_ignores_chat_base_override", func(t *testing.T) {
		a := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"g","realm":"global","domain":"www.workbuddy.ai","profile":"intl-cli"}`)
		var urls []string
		c := &Client{
			ProfilesEnabled: true,
			GlobalEnabled:   true,
			ChatBaseGlobal:  "https://chat.override",
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				urls = append(urls, r.URL.String())
				if strings.Contains(r.URL.Host, "codebuddy.ai") {
					return jsonResp(404, `{"code":404}`), nil
				}
				return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
			})},
		}
		if err := c.RefreshToken(a); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if len(urls) != 2 {
			t.Fatalf("attempts=%v want 2", urls)
		}
		if !strings.Contains(urls[1], "www.workbuddy.ai") {
			t.Errorf("fallback=%q want legacy www.workbuddy.ai (not the chat_base override)", urls[1])
		}
	})

	t.Run("inactive_single_candidate_legacy", func(t *testing.T) {
		// flag 非激活：候选序列必须是单元素旧 chatBase（不额外打 auth base）。
		a := mustProfileAuth(t, profileAuthJSON("cn-work"))
		var urls []string
		c := &Client{
			ProfilesDetectOnly: true,
			GlobalEnabled:      true,
			HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				urls = append(urls, r.URL.String())
				return jsonResp(404, `{"code":404}`), nil
			})},
			ChatBaseCN: "https://chat.example",
		}
		if err := c.RefreshToken(a); err == nil {
			t.Fatal("want error")
		}
		if len(urls) != 1 || !strings.HasPrefix(urls[0], "https://chat.example/") {
			t.Fatalf("attempts=%v want single legacy chatBase attempt", urls)
		}
	})
}

func jwtWithIss(t *testing.T, iss string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"iss": iss})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + "."
}

// TestProductProfileJWTIssCrossCheck iss 只做 host 级交叉校验：冲突时 iss 胜出并 warning，
// 缺失/不可解析时退回表。
func TestProductProfileJWTIssCrossCheck(t *testing.T) {
	t.Run("conflict_iss_wins_and_warns", func(t *testing.T) {
		var buf bytes.Buffer
		old := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(old)

		a := mustProfileAuth(t, `{"accessToken":"`+jwtWithIss(t, "https://www.workbuddy.ai")+`","refreshToken":"rt","uid":"c","domain":"copilot.tencent.com"}`)
		c, cap := newProfileCaptureClient(nil)
		if got := c.profileAuthBase(a); got != "https://www.workbuddy.ai" {
			t.Fatalf("auth base=%q want iss-derived www.workbuddy.ai", got)
		}
		if !strings.Contains(buf.String(), "iss") {
			t.Errorf("conflict must log a warning mentioning iss, got %q", buf.String())
		}
		mustRefresh(t, c, a)
		if got := urlHost(t, cap.urls[0]); got != "www.workbuddy.ai" {
			t.Errorf("refresh host=%q want iss-derived www.workbuddy.ai", got)
		}
	})

	t.Run("missing_iss_uses_table", func(t *testing.T) {
		a := mustProfileAuth(t, profileAuthJSON("cn-work"))
		c, _ := newProfileCaptureClient(nil)
		if got := c.profileAuthBase(a); got != "https://www.codebuddy.cn" {
			t.Errorf("auth base=%q want table codebuddy.cn", got)
		}
	})

	t.Run("unparseable_iss_uses_table", func(t *testing.T) {
		a := mustProfileAuth(t, `{"accessToken":"not-a.jwt.token","refreshToken":"rt","uid":"c2","domain":"copilot.tencent.com"}`)
		c, _ := newProfileCaptureClient(nil)
		if got := c.profileAuthBase(a); got != "https://copilot.tencent.com" {
			t.Errorf("auth base=%q want table copilot.tencent.com", got)
		}
	})
}

// TestProductProfileGlobalDisabledForcesCN global.enabled=false 时 intl-* 回落 cn-*（逃生门）。
func TestProductProfileGlobalDisabledForcesCN(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	cases := []struct {
		profile string
		id      string
		chat    string
		billing string
		web     string
		auth    string
	}{
		{"intl-cli", "cn-cli", "https://copilot.tencent.com", "https://www.codebuddy.cn", "https://www.workbuddy.cn", "https://copilot.tencent.com"},
		{"intl-work", "cn-work", "https://www.workbuddy.cn", "https://www.codebuddy.cn", "https://www.workbuddy.cn", "https://www.codebuddy.cn"},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			a := mustProfileAuth(t, profileAuthJSON(tc.profile))
			c, _ := newProfileCaptureClient(func(c *Client) { c.GlobalEnabled = false })
			desc, ok := c.ProfileFor(a)
			if !ok || desc.ID != tc.id {
				t.Fatalf("profile=(%q,%v) want (%q,true)", desc.ID, ok, tc.id)
			}
			if got := c.chatBase(a); got != tc.chat {
				t.Errorf("chat=%q want %q", got, tc.chat)
			}
			if got := c.billingBase(a); got != tc.billing {
				t.Errorf("billing=%q want %q", got, tc.billing)
			}
			if got := c.webBase(a); got != tc.web {
				t.Errorf("web=%q want %q", got, tc.web)
			}
			if got := c.profileAuthBase(a); got != tc.auth {
				t.Errorf("auth=%q want %q", got, tc.auth)
			}
			if got := c.userAgent(a); !strings.Contains(got, "WorkBuddy/5.5.4 WorkBuddy/5.5.4") {
				t.Errorf("UA=%q want CN platform after escape hatch", got)
			}
		})
	}
}

// TestProductProfileAuxiliaryBases 辅助端点：/v3/config、tasks、travel、desktop 走 chat base；
// webBase 恒取表列（cn-cli 固定 www.workbuddy.cn，不被任何 override 影响）。
func TestProductProfileAuxiliaryBases(t *testing.T) {
	cases := []struct {
		profile  string
		chatHost string
		webHost  string
	}{
		{"cn-cli", "copilot.tencent.com", "www.workbuddy.cn"},
		{"cn-work", "www.workbuddy.cn", "www.workbuddy.cn"},
		{"intl-cli", "www.codebuddy.ai", "www.codebuddy.ai"},
		{"intl-work", "www.workbuddy.ai", "www.workbuddy.ai"},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			a := mustProfileAuth(t, profileAuthJSON(tc.profile))
			c, cap := newProfileCaptureClient(func(c *Client) {
				// 所有 override 都设上：chat 生效、web 必须不受影响。
				c.ChatBaseGlobal = "https://chat.override"
				c.ChatBaseCN = "https://cn.override"
			})

			_, _ = c.fetchV3Models(a)                                   // /v3/config
			_, _ = c.TravelStatus(a)                                    // travel
			_, _ = c.ListTasks(a)                                       // tasks
			_ = c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "x"}) // desktop

			// 除 v3 外，所有请求都应落在「生效的 chat host」（含 override）。
			effChat := cap.hosts(t)[0]
			for i, host := range cap.hosts(t) {
				if host != effChat {
					t.Errorf("request[%d] host=%q want chat host %q (all=%v)", i, host, effChat, cap.hosts(t))
				}
			}
			// webBase 不受 override 影响。
			if got := urlHost(t, c.webBase(a)); got != tc.webHost {
				t.Errorf("webBase=%q want %q (must ignore overrides)", got, tc.webHost)
			}
		})
	}
}

// TestProductProfileWebBaseNeverOverridden 显式断言：WebBaseCN / global base 覆盖都不改 webBase。
func TestProductProfileWebBaseNeverOverridden(t *testing.T) {
	a := mustProfileAuth(t, profileAuthJSON("cn-cli"))
	c, _ := newProfileCaptureClient(func(c *Client) {
		c.WebBaseCN = "https://web.override"
		c.ChatBaseGlobal = "https://chat.override"
	})
	if got := c.webBase(a); got != "https://www.workbuddy.cn" {
		t.Errorf("webBase=%q want table www.workbuddy.cn (never overridden)", got)
	}
}

// TestProductProfileCapsMatrix Caps 取值以仓库现有门控为准；非激活时不可用。
func TestProductProfileCapsMatrix(t *testing.T) {
	all := ProductCaps{Checkin: true, Travel: true, School: true, BlackCat: true, GrowthDaily: true, Billing: true}
	cnOnlyBilling := ProductCaps{Billing: true}
	cases := []struct {
		profile string
		want    ProductCaps
	}{
		{"cn-cli", all},
		{"cn-work", all},
		{"intl-cli", cnOnlyBilling},
		{"intl-work", cnOnlyBilling},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			a := mustProfileAuth(t, profileAuthJSON(tc.profile))
			c, _ := newProfileCaptureClient(nil)
			desc, ok := c.ProfileFor(a)
			if !ok {
				t.Fatal("profiles should be active")
			}
			if desc.Caps != tc.want {
				t.Errorf("caps=%+v want %+v", desc.Caps, tc.want)
			}
		})
	}

	a := mustProfileAuth(t, profileAuthJSON("cn-cli"))
	c, _ := newProfileCaptureClient(func(c *Client) { c.ProfilesDetectOnly = true })
	if _, ok := c.ProfileFor(a); ok {
		t.Error("detect_only=true 时 Caps 不得参与门控")
	}
}

// TestProductProfileSelectionChain 选择链在路由层生效：explicit > derived(flag) > realm/domain。
func TestProductProfileSelectionChain(t *testing.T) {
	t.Run("explicit_beats_realm", func(t *testing.T) {
		a := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"x","realm":"global","profile":"cn-cli"}`)
		c, _ := newProfileCaptureClient(nil)
		if got := c.chatBase(a); got != "https://copilot.tencent.com" {
			t.Errorf("chat=%q want explicit cn-cli", got)
		}
	})
	t.Run("derived_requires_backfill_flag", func(t *testing.T) {
		withFlag := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"x","realm":"cn","profile_derived":"intl-cli","profile_backfill_v1":true}`)
		withoutFlag := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"x","realm":"cn","profile_derived":"intl-cli"}`)
		c, _ := newProfileCaptureClient(nil)
		if got := c.chatBase(withFlag); got != "https://www.codebuddy.ai" {
			t.Errorf("with flag chat=%q want derived intl-cli", got)
		}
		if got := c.chatBase(withoutFlag); got != "https://copilot.tencent.com" {
			t.Errorf("without flag chat=%q want realm cn-cli", got)
		}
	})
	t.Run("domain_table", func(t *testing.T) {
		a := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"x","domain":"www.workbuddy.cn"}`)
		c, _ := newProfileCaptureClient(nil)
		if got := c.chatBase(a); got != "https://www.workbuddy.cn" {
			t.Errorf("chat=%q want domain cn-work", got)
		}
	})
}

// TestProductProfileOriginSplit web/task 用 profile Origin；refresh/billing 保持 realm 判据
// （不因 profile 翻转）。billing 请求历史上不设 Origin，故只要求「未被翻转」。
func TestProductProfileOriginSplit(t *testing.T) {
	const realmOriginCN = "https://www.codebuddy.cn"

	t.Run("cn_work_web_profile_origin_refresh_realm_origin", func(t *testing.T) {
		a := mustProfileAuth(t, profileAuthJSON("cn-work"))
		c, cap := newProfileCaptureClient(nil)
		mustWeb(t, c, a)
		mustBilling(t, c, a)
		mustRefresh(t, c, a)
		if got := cap.origin[0]; got != "https://www.workbuddy.cn" {
			t.Errorf("web Origin=%q want profile cn-work Origin www.workbuddy.cn", got)
		}
		if got := cap.origin[2]; got != realmOriginCN {
			t.Errorf("refresh Origin=%q want realm-based %s (not flipped)", got, realmOriginCN)
		}
		if got := cap.origin[1]; got != "" && got != realmOriginCN {
			t.Errorf("billing Origin=%q want empty or realm-based %s (not flipped)", got, realmOriginCN)
		}
	})

	t.Run("intl_cli_web_profile_origin_others_realm_origin", func(t *testing.T) {
		// realm=global + explicit intl-cli：web Origin 取 profile（codebuddy.ai），
		// refresh/billing 仍取 realm（www.workbuddy.ai）——证明 originRefererFor 未被翻转。
		a := mustProfileAuth(t, `{"accessToken":"at","refreshToken":"rt","uid":"g","realm":"global","domain":"www.workbuddy.ai","profile":"intl-cli"}`)
		c, cap := newProfileCaptureClient(nil)
		mustWeb(t, c, a)
		mustBilling(t, c, a)
		mustRefresh(t, c, a)
		if got := cap.origin[0]; got != "https://www.codebuddy.ai" {
			t.Errorf("web Origin=%q want profile intl-cli Origin www.codebuddy.ai", got)
		}
		if got := cap.origin[2]; got != "https://www.workbuddy.ai" {
			t.Errorf("refresh Origin=%q want realm-based www.workbuddy.ai", got)
		}
		if got := cap.origin[1]; got != "" && got != "https://www.workbuddy.ai" {
			t.Errorf("billing Origin=%q want empty or realm-based www.workbuddy.ai", got)
		}
	})
}

// tableFor 仅测试用：按 profile 名查契约表期望值（避免测试内重复手写表）。
func tableFor(profile string) ProductProfile {
	desc, ok := productProfileTable[profile]
	if !ok {
		panic("unknown profile " + profile)
	}
	return desc
}
