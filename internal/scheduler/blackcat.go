// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
//
// Go 原生实现（接替旧 python 脚本 task_runner.py ALL --yes --only black_cat）：
// 去掉外部解释器依赖，语义不变（窗口判定 → BlackcatNeed → RunNightChats）。
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// nowForNightWindow 夜猫子窗口判定所用时钟（生产恒为 time.Now）。
//
// 之所以留一个可替换点：窗口守卫**先于**账号循环，若不钉住时钟，「企业号门控是否生效」
// 这条断言的判别力就完全取决于运行环境的墙钟——本机白天跑等于该用例什么都没测，
// 只有 CI 恰好落在 UTC 夜里才真正执行。属典型的「随环境静默失效」的测试。
var nowForNightWindow = time.Now

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。人工入口。
// 排程入口是 runBlackcat（cat 任务；schedule.cat_hours，默认 [1] 夜猫窗口内）。
func (s *Scheduler) RunBlackcatNow() { s.runBlackcat(triggerManual) }

// RunCatNow RunBlackcatNow 的既有别名（admin 任务入口与任务中心沿用 cat 名）。
func (s *Scheduler) RunCatNow() { s.runBlackcat(triggerManual) }

// runBlackcat 执行夜猫子任务并记台账（cat 排程入口）。
func (s *Scheduler) runBlackcat(trigger string) {
	started := time.Now()
	ok, fail, skip := s.blackcatAccountPass()
	s.recordRun(taskCat, trigger, started, runTally{
		total: ok + fail + skip, ok: ok, fail: fail, skipped: skip,
	})
}

// blackcatAccountPass 对池内可用账号补足夜间对话，返回 (ok, fail, skip) 计数。
// 窗口外直接返回全零（台账 total=0：本轮无事可做，非失败，不触发全灭重试）。
func (s *Scheduler) blackcatAccountPass() (ok, fail, skip int) {
	if !upstream.InNightWindow(nowForNightWindow()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return 0, 0, 0
	}
	for _, st := range s.cfg.Pool.List() {
		// 暂停选号（paused）账号跳过：夜猫子是全任务体系中唯一「整任务都是
		// 真实模型对话」的（RunNightChats 逐条 ChatStream 发 glm-5.2 短对话），
		// 与「让位防风控」正面冲突。旅行/成长任务都是纯上报或领奖 RPC，照常跑。
		if st.Disabled || st.Paused {
			skip++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			skip++
			continue
		}
		if a.IsGlobal() {
			skip++ // D4 门控：global 无 CN 任务体系，不发起任何上游调用
			continue
		}
		if a.IsEnterprise() {
			skip++ // 企业版门控：无成长体系（growth 403 / claim-gift 400「企业账号不支持该操作」）
			continue
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			fail++
			continue
		}
		if need <= 0 {
			ok++ // 已达标：无需补对话，本轮算成功
			continue
		}
		done, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.Nickname), done, need, err)
			fail++
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.Nickname), done)
		ok++
		time.Sleep(activityAccountDelay)
	}
	return ok, fail, skip
}
