package scheduler

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CheckinPlan 单账号单日的签到计划与尝试状态。
type CheckinPlan struct {
	UID       string    `json:"uid"`
	Day       string    `json:"day"`
	PlanAt    time.Time `json:"plan_at"`
	RetryAt   time.Time `json:"retry_at"`
	Attempts  int       `json:"attempts"`
	State     string    `json:"state"` // pending / done / failed / skipped
	LastRunAt time.Time `json:"last_run_at,omitempty"`
}

type checkinPlanFile struct {
	Accounts map[string]CheckinPlan `json:"accounts"`
}

// PlanStore 持久化 random 签到计划（data/checkin-plan.json）。
type PlanStore struct {
	mu    sync.Mutex
	path  string
	plans map[string]CheckinPlan
}

func planKey(uid, day string) string { return uid + "|" + day }

func NewPlanStore(path string) *PlanStore {
	s := &PlanStore{path: path, plans: map[string]CheckinPlan{}}
	s.load()
	return s
}

func (s *PlanStore) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var f checkinPlanFile
	if json.Unmarshal(raw, &f) != nil || f.Accounts == nil {
		return
	}
	s.plans = f.Accounts
}

func (s *PlanStore) saveLocked() {
	if s.path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	raw, err := json.MarshalIndent(checkinPlanFile{Accounts: s.plans}, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// Ensure 返回账号当日计划；不存在时按窗口确定性生成并落盘。
func (s *PlanStore) Ensure(uid, day string, now time.Time, windowStart, windowEnd, retryStart, retryEnd time.Time) CheckinPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := planKey(uid, day)
	if p, ok := s.plans[key]; ok {
		return p
	}
	span := int(windowEnd.Sub(windowStart).Minutes())
	if span <= 0 {
		span = 1
	}
	retrySpan := int(retryEnd.Sub(retryStart).Minutes())
	if retrySpan <= 0 {
		retrySpan = 1
	}
	p := CheckinPlan{
		UID: uid, Day: day, State: "pending",
		PlanAt:  windowStart.Add(time.Duration(planHash(uid+day)%int64(span)) * time.Minute),
		RetryAt: retryStart.Add(time.Duration(planHash(uid+day+"retry")%int64(retrySpan)) * time.Minute),
	}
	s.plans[key] = p
	s.saveLocked()
	return p
}

// Due 返回当前应执行的计划：首次未尝试且已到计划时间，或失败一次且已到重试时间。
func (s *PlanStore) Due(now time.Time, retryEnd time.Time) []CheckinPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := travelDay(now)
	out := make([]CheckinPlan, 0)
	for key, p := range s.plans {
		if p.Day != day {
			continue
		}
		switch {
		case p.State == "pending" && p.Attempts == 0 && !now.Before(p.PlanAt):
			out = append(out, p)
		case p.State == "failed" && p.Attempts == 1 && !now.Before(p.RetryAt) && !now.After(retryEnd):
			out = append(out, p)
		}
		_ = key
	}
	return out
}

// MarkAttempt 在请求前占用一次尝试（进程中断也不重发）。
func (s *PlanStore) MarkAttempt(uid, day string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := planKey(uid, day)
	p := s.plans[key]
	p.Attempts++
	p.LastRunAt = now
	s.plans[key] = p
	s.saveLocked()
}

// SetState 更新最终状态。
func (s *PlanStore) SetState(uid, day, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := planKey(uid, day)
	p := s.plans[key]
	p.State = state
	s.plans[key] = p
	s.saveLocked()
}

// Get 读取计划。
func (s *PlanStore) Get(uid, day string) (CheckinPlan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planKey(uid, day)]
	return p, ok
}

// Prune 删除非今日计划，避免文件无限增长。
func (s *PlanStore) Prune(day string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, p := range s.plans {
		if p.Day != day {
			delete(s.plans, key)
		}
	}
	s.saveLocked()
}

func planHash(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// parseClock 解析 HH:MM 为当天时刻（CST）。
func parseClock(day, hhmm string) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(hhmm), ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("时间格式应为 HH:MM")
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return time.Time{}, err
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return time.Time{}, err
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return time.Time{}, fmt.Errorf("时间格式应为 HH:MM")
	}
	d, err := time.ParseInLocation("2006-01-02", day, cstZone)
	if err != nil {
		return time.Time{}, err
	}
	return d.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute), nil
}
