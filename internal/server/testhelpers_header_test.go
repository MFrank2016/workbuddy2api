package server

import (
	"net/http"
	"strings"
)

// canonicalHeaderCloneServer 把捕获到的出站头键归一成 canonical 形式。
// 生产代码已按官方拼写重排（upstream/headcase.go：X-Conversation-ID 等非
// canonical key），而测试断言习惯用 Header.Get（内部会把查询键 canonicalize）。
func canonicalHeaderCloneServer(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[http.CanonicalHeaderKey(k)] = v
	}
	return out
}

// headerFoldServer 大小写不敏感读取（生产头名已按官方拼写重排，Get 会 canonicalize
// 查询键而取不到值）。
func headerFoldServer(h http.Header, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
