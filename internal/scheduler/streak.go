// streak.go 连登奖励管家：对池内账号执行「礼包/补偿 → 连登兑换 → 抽奖」闭环。
//
// 实现复用 claimGrowthRewards（定义见 scheduler.go，本地单一口径）：按自然日幂等
// （rewardClaimedToday），已领/未达标/无次数自动跳过。合流 panel 时带入的
// GrowthStreakFull 独立实现（streakBonusAccount）已删除——与 claimGrowthRewards
// 双轨重复，属死代码；上游 GrowthStreakFull/GrowthRedeemTier 仍保留在 upstream
// 包内（API 面完整、有独立测试），只是调度侧不再走那条路径。
//
// 入口：RunStreakBonusNow（人工/任务中心/企业门控测试）。定时口径在 runActivity
// 尾部（点亮连登后按天领取，见 claimGrowthRewards 注释）。
package scheduler

// RunStreakBonusNow 对所有可用账号执行连登奖励领取（幂等：已完成/无次数自动跳过）。
func (s *Scheduler) RunStreakBonusNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		if a.IsEnterpriseSession() {
			continue // 企业版 / 企业切换会话门控：无成长体系（/activity/growth/streak 企业号 403；切换会话 200 但不计分）
		}
		s.claimGrowthRewards(a)
	}
}
