package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
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
