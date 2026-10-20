// retry.go 聊天 SSE 的**同账号瞬时重试**（移植 workbuddy-switch-plus v0.1.3
// mirror.py 的三条硬纪律：默认关 / 只重试幂等无副作用的失败 / 退避必须有上限）。
package upstream

import (
	"context"
	"math/rand/v2"
	"time"
)

// RetryConfig 同号瞬时重试配置。零值 = 未开启（Enabled=false）。
type RetryConfig struct {
	Enabled  bool
	Attempts int           // 第 1 次失败后的额外重试次数，钳 [1,5]
	Backoff  time.Duration // 退避基数，钳 [0.25s,8s]
}

// retryJitterFraction 重试退避抖动比例（±25%，与 server/backoff.go 的
// jitterDur 同风格——打散多请求同相位重试，WAF 频控按密度判罚）。
const retryJitterFraction = 0.25

// retryBackoffCap 重试退避封顶（对齐 mirror MAX_BACKOFF_CAP=8s）。
const retryBackoffCap = 8 * time.Second

// retryAllowed 报告一次失败是否值得**同号**重试。
//
// 只重试两类：传输层失败（DNS/超时/连接重置——重试最值钱的场景）与
// ErrServer（5xx 上游故障）。429/402/401/403 等**永不重试**：配额重试只会
// 更快被限，凭据问题重试不会变得有权限（mirror mirror.py 同理由）。
func retryAllowed(kind ErrKind, transportErr bool) bool {
	if transportErr {
		return true
	}
	return kind == ErrServer
}

// retryBackoff 返回第 n 次重试（1 起）前的等待时长：base·2^(n-1) 封顶
// retryBackoffCap，再施加 ±25% 抖动。base<=0 返回 0（测试用，跳过等待）。
// 退避必须先封顶：重试次数叠乘退避会变成"卡住不返回"，用户看到的是超时。
func retryBackoff(base time.Duration, n int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for k := 1; k < n && d < retryBackoffCap; k++ {
		d *= 2
		if d <= 0 {
			break
		}
	}
	if d > retryBackoffCap {
		d = retryBackoffCap
	}
	f := 1 + (rand.Float64()*2-1)*retryJitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// sleepCtxRetry 可取消的重试退避等待：ctx 取消立即返回 false（客户端断连/
// 优雅停机不必等退避睡醒），等满返回 true。d<=0 立即放行。与 server 包的
// sleepCtx 同模式（按任务书「等价物」口径在消费侧实现，不反向依赖 server）。
func sleepCtxRetry(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
