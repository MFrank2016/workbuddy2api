package scheduler

import (
	"encoding/base64"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// stampedPersonalJWT 拼一个 token_source=enterprise_switch 的 JWT（只带此声明、
// enterpriseId 为空的个人号）。该声明只记录本次 OAuth 授权走了哪条流程，
// 不代表账号形态——2026-10-14 实测证伪「带此章的号行为事件被静默丢弃」，
// 详见 auth.IsEnterprise 的说明。本测试用它做回归钉子，防止有人再按 token
// 声明把个人号误判为企业上下文。
func stampedPersonalJWT() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1","token_source":"enterprise_switch","enterprise_id":""}`))
	return "hdr." + payload + ".sig"
}

// sessionTestScheduler 构造只含一个账号的调度器；stamped=true 时账号的 AccessToken
// 为带 token_source=enterprise_switch 的 JWT（个人号、enterpriseId 为空）；false 为
// 普通个人号对照组。两者都应照常发起行为类任务。
func sessionTestScheduler(t *testing.T, stamped bool) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	token := "at"
	if stamped {
		token = stampedPersonalJWT()
	}
	a := &auth.Auth{UID: "u1", AccessToken: token, RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	return s, r
}

// TestStampedTokenNotGated 回归钉子：带 token_source=enterprise_switch 的**个人号**
// （enterpriseId 为空）必须照常发起全部行为类任务，不得被按 token 声明门控跳过。
//
// 门控只认 enterpriseId（auth.IsEnterprise）；该 JWT 声明只记授权流程，2026-10-14
// 实测带此章的个人号 chat_5/expert_5/Library_read/Hp_Appearance 等 /v2/report 行为
// 事件全部正常计分领奖，签到 / 连登 / 旅行 / 夜猫子同样照常。原判定把它当「企业
// 切换会话」跳过，导致个人号每天白丢成长任务收益并弹出「请移除账号重新登录」。
// 每个子用例都配普通个人号对照，证明「发了请求」不是构造有误。
func TestStampedTokenNotGated(t *testing.T) {
	// 把夜猫子窗口钉在窗口内，否则本机白天跑该用例会空转（守卫先于账号循环）。
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 12, 23, 30, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name     string
		run      func(*Scheduler)
		endpoint string // 该任务在个人号上必然发起的上游路径
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin"},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 带声明的个人号：必须照常发请求
			sStamp, rStamp := sessionTestScheduler(t, true)
			c.run(sStamp)
			if !rStamp.has(c.endpoint) {
				t.Errorf("带 enterprise_switch 声明的个人号不应被门控跳过 %s，实际请求=%v", c.endpoint, rStamp.snapshot())
			}

			// 普通个人号对照：同样照常发请求
			sPlain, rPlain := sessionTestScheduler(t, false)
			c.run(sPlain)
			if !rPlain.has(c.endpoint) {
				t.Errorf("普通个人号对照应发起 %s，实际请求=%v", c.endpoint, rPlain.snapshot())
			}
		})
	}
}

// TestStampedTokenKeepsKeepalive 带声明的个人号照常保活（token 会过期，会话
// 形态不影响选号/刷新能力）。
func TestStampedTokenKeepsKeepalive(t *testing.T) {
	s, r := sessionTestScheduler(t, true)
	s.RunKeepaliveNow()
	if !r.has("/token/refresh") {
		t.Errorf("带声明的个人号必须照常保活，实际请求=%v", r.snapshot())
	}
}
