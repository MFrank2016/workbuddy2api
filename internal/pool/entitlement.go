package pool

import (
	"sort"
	"time"
)

// modelEntitlementTTL 模型权限观测有效期（11158 免费号不可用；24h 后自动过期）。
const modelEntitlementTTL = 24 * time.Hour

// ModelEntitlement 模型级权限观测（11158「model not allowed for user type」）。
// 纯观测：不参与选号（付费号可能可用），/status 与面板据此提示「免费账号不可用」。
type ModelEntitlement struct {
	Realm string    `json:"realm"`
	Model string    `json:"model"`
	Code  string    `json:"code,omitempty"`
	Until time.Time `json:"until,omitempty"`
}

type modelEntitlement struct {
	realm, model, code string
	until              time.Time
}

// NoteModelEntitlement 记录一次 11158（模型对当前账号类型不可用）。
// 24h 后自动过期；同一 (realm, model) 后续成功请求由 ClearModelEntitlement 撤标。
func (p *Pool) NoteModelEntitlement(realm, model, code string) {
	if model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.modelEntitlements == nil {
		p.modelEntitlements = map[string]modelEntitlement{}
	}
	p.modelEntitlements[realm+"|"+model] = modelEntitlement{
		realm: realm, model: model, code: code, until: time.Now().Add(modelEntitlementTTL),
	}
	p.dirty.Store(true)
}

// ClearModelEntitlement 撤掉 (realm, model) 标记（有成功请求证明该模型可用）。
func (p *Pool) ClearModelEntitlement(realm, model string) {
	if model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := realm + "|" + model
	if _, ok := p.modelEntitlements[k]; ok {
		delete(p.modelEntitlements, k)
		p.dirty.Store(true)
	}
}

// ModelEntitlements 返回未过期的观测（过期即消失，排序稳定）。
func (p *Pool) ModelEntitlements() []ModelEntitlement {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]ModelEntitlement, 0, len(p.modelEntitlements))
	for _, e := range p.modelEntitlements {
		if now.Before(e.until) {
			out = append(out, ModelEntitlement{Realm: e.realm, Model: e.model, Code: e.code, Until: e.until})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Realm != out[j].Realm {
			return out[i].Realm < out[j].Realm
		}
		return out[i].Model < out[j].Model
	})
	return out
}
