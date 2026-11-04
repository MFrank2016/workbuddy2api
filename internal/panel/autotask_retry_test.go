package panel

import (
	"errors"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestRunActionWithSoftRateRetryCallsActionOnce 一键完成/单任务动作撞 429 软限流时，
// runActionWithSoftRateRetry 应调用**传进来的动作**一次，只在软限流时补发一次；
// 绝不能递归调用自己——那会无限递归直到 stack overflow 打挂整个进程。
//
// 回归钉子：曾有版本把首行写成 `msg, err := p.runActionWithSoftRateRetry(a, act)`
// （自递归），导致任何进入该函数的任务动作都触发 fatal error: stack overflow。
// 该路径此前被「企业切换会话」误判的 422 门控挡住，修掉误判后才暴露。
func TestRunActionWithSoftRateRetryCallsActionOnce(t *testing.T) {
	p := &Panel{}
	called := 0
	act := &autoAction{
		TaskCode: "test_action",
		run: func(p *Panel, a *auth.Auth) (string, error) {
			called++
			return "done", nil
		},
	}

	msg, err := p.runActionWithSoftRateRetry(&auth.Auth{UID: "u1"}, act)
	if err != nil {
		t.Fatalf("runActionWithSoftRateRetry 返回错误: %v", err)
	}
	if msg != "done" {
		t.Errorf("msg = %q, want %q", msg, "done")
	}
	if called != 1 {
		t.Errorf("动作被调用 %d 次, want 1（自递归会立刻栈溢出或重复执行）", called)
	}
}

// TestRunActionWithSoftRateRetryRetriesOnceOnSoftRate 动作首次返回 429 软限流时应
// 等待窗口后补发一次；把 softRateGap 置 0 让测试不真的睡 15s。补发仍走 act.run，
// 不是再进一次自己。
func TestRunActionWithSoftRateRetryRetriesOnceOnSoftRate(t *testing.T) {
	oldGap := softRateGap
	softRateGap = 0
	t.Cleanup(func() { softRateGap = oldGap })

	p := &Panel{}
	called := 0
	act := &autoAction{
		TaskCode: "test_action",
		run: func(p *Panel, a *auth.Auth) (string, error) {
			called++
			if called == 1 {
				return "", &upstream.Error{Kind: upstream.ErrSoftRate, Msg: "rate limited"}
			}
			return "done", nil
		},
	}

	msg, err := p.runActionWithSoftRateRetry(&auth.Auth{UID: "u1"}, act)
	if err != nil {
		t.Fatalf("补发后应成功, 实际错误: %v", err)
	}
	if msg != "done" {
		t.Errorf("msg = %q, want %q", msg, "done")
	}
	if called != 2 {
		t.Errorf("动作被调用 %d 次, want 2（首次 + 软限流补发一次）", called)
	}
}

// TestRunActionWithSoftRateRetryDoesNotRetryOtherErrors 非软限流错误直接返回，
// 不补发、不递归。
func TestRunActionWithSoftRateRetryDoesNotRetryOtherErrors(t *testing.T) {
	p := &Panel{}
	want := errors.New("boom")
	called := 0
	act := &autoAction{
		TaskCode: "test_action",
		run: func(p *Panel, a *auth.Auth) (string, error) {
			called++
			return "", want
		},
	}

	_, err := p.runActionWithSoftRateRetry(&auth.Auth{UID: "u1"}, act)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if called != 1 {
		t.Errorf("动作被调用 %d 次, want 1", called)
	}
}
