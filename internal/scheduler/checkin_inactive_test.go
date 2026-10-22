// checkin_inactive_test.go 钉住签到三态在调度器侧的行为：
//
// 「活动未开启/已结束」是活动级正常态（上游活动下线），既不是账号故障，也不是
// 「今天已签到」。它必须：
//   - 记为 CheckinInactive（值 inactive），台账单列 Inactive 计数；
//   - **不计入 failN**（否则每天两轮签到都在报失败，运维会去查一个不存在的故障）；
//   - **不排 retry_delay_minutes 重试**（活动没开，重试多少次都一样，纯打上游）；
//   - **不喂熔断**（熔断器只认账号级失败；活动状态与账号健康无关）。
//
// 同时锁住判定顺序 inactive > already：文案同时含「已领取」时仍判 inactive。
package scheduler

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestCheckinAllInactive 活动未开启 → status=inactive，余额照常刷新。
func TestCheckinAllInactive(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":10001,"msg":"活动未开启"}`,
		checkinStatus:  http.StatusBadRequest,
		resourceRemain: 300,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].Status != CheckinInactive {
		t.Fatalf("out=%+v want inactive", out)
	}
	// 余额刷新不该被活动状态阻断（额度是账号健康的事实来源）。
	if out[0].Credits == nil || *out[0].Credits != 300 {
		t.Errorf("credits=%v want 300（inactive 仍应刷新余额）", out[0].Credits)
	}
	if st, _ := p.Status("u1"); st.Credits != 300 {
		t.Errorf("pool credits=%d want 300", st.Credits)
	}
}

// TestCheckinAllInactiveWithClaimedMarker inactive 文案同时含「已领取」时仍判
// inactive（顺序 inactive > already），不得退化成 already/fail。
func TestCheckinAllInactiveWithClaimedMarker(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":10001,"msg":"活动已结束，奖励已领取"}`,
		checkinStatus:  http.StatusBadRequest,
		resourceRemain: 100,
	})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinInactive {
		t.Errorf("status=%q want inactive（顺序 inactive > already）", out[0].Status)
	}
}

// TestCheckinAllInactiveDoesNotFailOrRetry 台账层：fail=0、inactive=1、不排重试、
// 不判全灭；熔断计数器不动、账号不进冷却。
func TestCheckinAllInactiveDoesNotFailOrRetry(t *testing.T) {
	stub := &checkinStub{
		checkinBody:    `{"code":10001,"msg":"活动已结束"}`,
		checkinStatus:  http.StatusBadRequest,
		resourceRemain: 100,
	}
	srv := stub.server()
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:              p,
		Upstream:          up,
		RetryDelayMinutes: 15, // 若被误判失败，全灭会排重试 —— 本断言就是要它不排
		RetryMaxPerDay:    3,
	})

	// 定时入口语义（runCheckin(triggerSchedule) 才会走重试判定）。
	s.runCheckin(triggerSchedule)

	r := s.Ledger().Runs()["checkin"]
	if r.Fail != 0 {
		t.Errorf("inactive 不得计入 fail（got fail=%d）", r.Fail)
	}
	if r.Inactive != 1 {
		t.Errorf("Inactive=%d want 1", r.Inactive)
	}
	if r.Total != r.OK+r.Already+r.Fail+r.Skipped+r.Inactive {
		t.Errorf("total=%d 与分解不守恒: %+v", r.Total, r)
	}
	if r.AllFailed {
		t.Error("inactive 不得判全灭")
	}
	if armed := s.Ledger().ArmedRetries(r.Finished); len(armed) != 0 {
		t.Errorf("inactive 不得排重试，got %v", armed)
	}
	st, _ := p.Status("u1")
	if st.BreakerFails != 0 || !st.BreakerUntil.IsZero() {
		t.Errorf("inactive 不得喂熔断：fails=%d until=%v", st.BreakerFails, st.BreakerUntil)
	}
	if st.Cooling {
		t.Errorf("inactive 不得冷却账号：%+v", st)
	}
}

// TestCheckinAllAlreadyStillAlready code=10001 +「今天已签到」仍判 already
// （不能被新加的 inactive 分支吞掉）。
func TestCheckinAllAlreadyStillAlready(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":10001,"msg":"今天已签到"}`,
		checkinStatus:  http.StatusBadRequest,
		resourceRemain: 100,
	})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinAlready {
		t.Errorf("status=%q want already", out[0].Status)
	}
	if out[0].Detail != "" {
		t.Errorf("already 不应回填 detail，got %q", out[0].Detail)
	}
}

// TestCheckinAllTokenExpiredIsFail 凭证过期必须仍是 fail（既非 already 也非 inactive）。
func TestCheckinAllTokenExpiredIsFail(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":12153,"msg":"access token 已过期"}`,
		checkinStatus:  http.StatusUnauthorized,
		resourceRemain: 100,
	})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail（凭证过期不是活动结束）", out[0].Status)
	}
}

// TestCheckinAllNetworkErrorIsFail 网络/解析错误既非 already 也非 inactive。
func TestCheckinAllNetworkErrorIsFail(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `not json at all`,
		resourceRemain: 100,
	})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail（解析错误不得判 already/inactive）", out[0].Status)
	}
}
