package server

import "strings"

// resolveModelDetail 解析模型名协议（PLAN D6），并区分「realm 是否由调用方显式写出」：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 第三个返回值 realmExplicit 供跨域回退（issue #135）判定：只有裸名（false）才允许
// 在首选域选不到号时改用另一域；显式 cn:/global: 前缀永不被回退改写。
func resolveModelDetail(model string) (realm, bare string, realmExplicit bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model, false
	}
	return prefix, model[idx+1:], true
}

// resolveModel 是 resolveModelDetail 的两返回值形态（既有调用方语义零改动）。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelDetail(model)
	return realm, bare
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// matchModelAlias 在 aliases 中为 model 找重定向目标：先精确匹配，再按后缀 "*"
// 通配匹配（取最长前缀 = 最具体者）；无匹配返回 ""（不启用）。
func matchModelAlias(aliases map[string]string, model string) string {
	if len(aliases) == 0 || model == "" {
		return ""
	}
	if v, ok := aliases[model]; ok {
		return v
	}
	bestPrefix, best := "", ""
	for pat, v := range aliases {
		if !strings.HasSuffix(pat, "*") {
			continue
		}
		prefix := strings.TrimSuffix(pat, "*")
		if prefix == "" || !strings.HasPrefix(model, prefix) {
			continue
		}
		if len(prefix) > len(bestPrefix) {
			bestPrefix, best = prefix, v
		}
	}
	return best
}

// matchModelPatterns 判断 model 是否命中白名单（精确或后缀 "*" 通配）。
func matchModelPatterns(patterns []string, model string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == model {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// otherRealm 返回另一个选号域（"cn" ↔ "global"），供跨域回退使用。
func otherRealm(realm string) string {
	if realm == "global" {
		return "cn"
	}
	return "global"
}
