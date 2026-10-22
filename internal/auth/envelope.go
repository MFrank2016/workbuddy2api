package auth

import (
	"encoding/json"
	"fmt"
)

// encryptedAuthError 是 WorkBuddy 5.6+ 客户端 at-rest 加密凭据的**文档化**错误串。
// 逐字固定：上游文档与 manager 的排障指引都按这一串匹配，改一个字就会让
// 「该去 manager 导入」的提示失效。
const encryptedAuthError = "auth_encrypted_by_client: WorkBuddy 5.6+ 客户端 at-rest 加密凭据，请通过 manager 导入而不是直接拷贝桌面客户端文件"

// 信封标记键：客户端把明文字段换成的对象形状。
const (
	encryptedMarkerKey   = "$wbEncrypted"
	encryptedEnvelopeKey = "envelope"
)

// encryptedAuthErrorValue 返回文档化的信封错误（供 Parse 使用）。
func encryptedAuthErrorValue() error { return fmt.Errorf("%s", encryptedAuthError) }

// hasEncryptedEnvelope 递归扫描已解码的 JSON 树，判断是否存在
// {"$wbEncrypted":1,"envelope":"..."} 形状的值。
//
// 为什么必须递归：客户端 5.6+ 只加密「有值」的字段，同一份文件里可能
// accessToken 是信封、refreshToken 还是明文；信封也可能被包在 account 的
// 嵌套对象或数组里（版本演进不保证扁平）。只看固定字段会漏掉一半形态，
// 表现为「文件能读、请求才失败」——最贵的排查路径。
func hasEncryptedEnvelope(v any) bool {
	switch node := v.(type) {
	case map[string]any:
		if _, ok := node[encryptedMarkerKey]; ok {
			if _, hasEnv := node[encryptedEnvelopeKey]; hasEnv {
				return true
			}
		}
		for _, child := range node {
			if hasEncryptedEnvelope(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if hasEncryptedEnvelope(child) {
				return true
			}
		}
	}
	return false
}

// rejectEncryptedEnvelope 在反序列化前检查磁盘内容是否含客户端加密信封。
//
// ★ 不实现解密 ★：解开信封需要 Electron native binding 里的密钥（由客户端
// 主进程持有），core 是纯 Go 运行时，取不到那把钥。因此这里只做**识别 + 拒绝**，
// 并给出可执行的动作（通过 manager 导入），而不是尝试猜测或降级为空 token。
func rejectEncryptedEnvelope(raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 无法解码的输入交给后续 probe 统一报 storage_parse_error。
		return nil
	}
	if hasEncryptedEnvelope(doc) {
		return encryptedAuthErrorValue()
	}
	return nil
}
