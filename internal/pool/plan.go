package pool

import "time"

// Plan 套餐展示快照（不参与选号）。
type Plan struct {
	Label     string
	Stale     bool
	CheckedAt time.Time
}

// SetPlan 写入账号套餐识别结果并标记落盘。账号不存在时静默忽略。
func (p *Pool) SetPlan(uid string, plan Plan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.planLabel = plan.Label
	e.planStale = plan.Stale
	e.planCheckedAt = plan.CheckedAt
	p.dirty.Store(true)
}
