// inactive_checkin_test.go 钉住签到三态判定（inactive > already > ok）：
// 「活动未开启/已结束」类文案是活动级正常态，不是账号故障，也不是「今天已签到」；
// 「token 已过期」这类凭证文案绝不能被误判为 inactive（负向守卫：必须同时提到
// 活动/签到）。网络层/解析层错误既不判 already 也不判 inactive。
package upstream

import (
	"errors"
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// bizErr 造一条带分类的上游业务错误（与 doJSON 的形态一致）。
func bizErr(msg string) error {
	return &Error{Kind: ErrClient, Status: http.StatusBadRequest, Msg: msg}
}

func TestIsInactiveCheckin(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// 活动未开启 / 未开始 / 未开放 / 已结束 / 已关闭 / 已暂停。
		{name: "活动未开启", err: bizErr(`{"code":10001,"msg":"活动未开启"}`), want: true},
		{name: "活动已结束", err: bizErr(`{"code":10001,"msg":"活动已结束"}`), want: true},
		{name: "活动已结束+奖励已领取", err: bizErr(`{"code":10001,"msg":"活动已结束，奖励已领取"}`), want: true},
		{name: "签到活动未开始", err: bizErr(`{"code":10001,"msg":"签到活动未开始"}`), want: true},
		{name: "活动已暂停", err: bizErr(`{"code":10001,"msg":"当前活动已暂停"}`), want: true},
		// 过期类：必须同时提到签到/活动。
		{name: "签到已过期", err: bizErr(`{"code":10001,"msg":"签到已过期"}`), want: true},
		{name: "活动已过期", err: bizErr(`{"code":10001,"msg":"活动已过期"}`), want: true},
		// 无活动类。
		{name: "当前无进行中的活动", err: bizErr(`{"code":10001,"msg":"当前无进行中的活动"}`), want: true},
		// 负向守卫：凭证过期没有提到活动/签到 → 不是 inactive。
		{name: "token 已过期", err: bizErr(`{"code":10001,"msg":"token 已过期"}`), want: false},
		{name: "access token 已过期", err: bizErr(`{"msg":"access token 已过期"}`), want: false},
		{name: "session 已过期", err: bizErr(`{"code":12153,"msg":"session 已过期"}`), want: false},
		// 今天已签到不是 inactive。
		{name: "今天已签到", err: bizErr(`{"code":10001,"msg":"今天已签到"}`), want: false},
		{name: "今日已签到", err: bizErr(`{"code":14001,"msg":"今日已签到"}`), want: false},
		// 其它业务失败不是 inactive。
		{name: "余额不足", err: bizErr(`{"code":10002,"msg":"积分不足，请充值"}`), want: false},
		{name: "空 msg", err: bizErr(""), want: false},
		// 网络层/解析层错误（非 *Error）绝不判 inactive。
		{name: "网络错误", err: errors.New("dial tcp: connection refused"), want: false},
		{name: "解析错误", err: errors.New("parse failed: unexpected EOF"), want: false},
		{name: "裸错误带文案", err: errors.New("活动未开启"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsInactiveCheckin(c.err); got != c.want {
				t.Errorf("IsInactiveCheckin(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}
}

// TestIsAlreadyCheckinYieldsToInactive 判定顺序 inactive > already：
// 文案同时含「已签到」与「活动已结束」时，IsAlreadyCheckin 必须让位。
func TestIsAlreadyCheckinYieldsToInactive(t *testing.T) {
	err := bizErr(`{"code":10001,"msg":"签到活动已结束，今天已签到"}`)
	if !IsInactiveCheckin(err) {
		t.Fatal("含活动已结束应判 inactive")
	}
	if IsAlreadyCheckin(err) {
		t.Error("inactive 命中时 IsAlreadyCheckin 不得再判 already（顺序 inactive > already）")
	}
	// 纯「今天已签到」仍只判 already，不判 inactive。
	al := bizErr(`{"code":10001,"msg":"今天已签到"}`)
	if !IsAlreadyCheckin(al) || IsInactiveCheckin(al) {
		t.Errorf("今天已签到应 already only: already=%v inactive=%v",
			IsAlreadyCheckin(al), IsInactiveCheckin(al))
	}
}

// TestDailyCheckinInactiveResponse 走真实响应路径：inactive 文案产出 *Error，
// 两个判定函数各司其职；inactive 不得被当成 already（否则台账把活动结束记成
// 「今天已签到」，账号当天实际没签却显示正常）。
func TestDailyCheckinInactiveResponse(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"code":10001,"msg":"活动已结束，奖励已领取"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("业务错误应返回 error")
	}
	if !IsInactiveCheckin(err) {
		t.Error("活动已结束 应判 inactive")
	}
	if IsAlreadyCheckin(err) {
		t.Error("活动已结束 不得判 already")
	}
}

// TestDailyCheckinTokenExpiredNotInactive 凭证过期是失败，不是活动结束。
func TestDailyCheckinTokenExpiredNotInactive(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, `{"code":12153,"msg":"access token 已过期"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("凭证过期应返回 error")
	}
	if IsInactiveCheckin(err) {
		t.Error("access token 已过期 不得判 inactive")
	}
	if IsAlreadyCheckin(err) {
		t.Error("access token 已过期 不得判 already")
	}
}
