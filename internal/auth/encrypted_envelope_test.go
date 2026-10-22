package auth

import (
	"strings"
	"testing"
)

// wantEncryptedAuthError 是客户端 at-rest 加密凭据的**文档化**错误串（逐字，不得改写）。
const wantEncryptedAuthError = "auth_encrypted_by_client: WorkBuddy 5.6+ 客户端 at-rest 加密凭据，请通过 manager 导入而不是直接拷贝桌面客户端文件"

// assertEncryptedEnvelopeRejected 断言 err 是文档化的信封错误：
//   - 文本与 wantEncryptedAuthError 完全一致；
//   - 不再表现为 storage_parse_error / missing accessToken（那两条会把用户引向
//     「文件坏了」或「少写了 token」的错误排查方向）。
func assertEncryptedEnvelopeRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want auth_encrypted_by_client error, got nil (envelope silently accepted)")
	}
	if got := err.Error(); got != wantEncryptedAuthError {
		t.Errorf("error text = %q, want exactly %q", got, wantEncryptedAuthError)
	}
	for _, forbidden := range []string{"storage_parse_error", "missing accessToken"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("error %q must not surface as %q", err.Error(), forbidden)
		}
	}
}

// TestParseRejectsEncryptedEnvelope 覆盖 WorkBuddy 5.6+ 客户端把凭据换成
// {"$wbEncrypted":1,"envelope":"..."} 信封后的磁盘形态：必须在反序列化前识别，
// 并给出「请通过 manager 导入」的文档化错误（不解密——取钥需 Electron native binding）。
func TestParseRejectsEncryptedEnvelope(t *testing.T) {
	t.Run("nested accessToken envelope", func(t *testing.T) {
		raw := []byte(`{"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"AAA"},"refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1"}}`)
		_, err := Parse(raw)
		assertEncryptedEnvelopeRejected(t, err)
	})

	t.Run("nested refreshToken envelope", func(t *testing.T) {
		raw := []byte(`{"auth":{"accessToken":"at","refreshToken":{"$wbEncrypted":1,"envelope":"BBB"},"expiresAt":1,"domain":""},"account":{"uid":"u1"}}`)
		_, err := Parse(raw)
		assertEncryptedEnvelopeRejected(t, err)
	})

	// 信封覆盖 nickname/phoneNumber：只有 accessToken 明文时也必须命中，
	// 否则账号会带着「半加密」凭据被加载，后续上游请求才失败。
	t.Run("account nickname envelope", func(t *testing.T) {
		raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1","nickname":{"$wbEncrypted":1,"envelope":"CCC"}}}`)
		_, err := Parse(raw)
		assertEncryptedEnvelopeRejected(t, err)
	})

	t.Run("account phoneNumber envelope", func(t *testing.T) {
		raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1","phoneNumber":{"$wbEncrypted":1,"envelope":"DDD"}}}`)
		_, err := Parse(raw)
		assertEncryptedEnvelopeRejected(t, err)
	})

	// 扁平形（手写/旧版）同样可能被客户端加密字段污染：扫描不能只认嵌套形。
	t.Run("flat accessToken envelope", func(t *testing.T) {
		raw := []byte(`{"accessToken":{"$wbEncrypted":1,"envelope":"EEE"},"uid":"u2"}`)
		_, err := Parse(raw)
		assertEncryptedEnvelopeRejected(t, err)
	})

	// 递归：信封可能被包在更深的对象/数组里（客户端版本演进时不保证扁平）。
	t.Run("deeply nested envelope", func(t *testing.T) {
		raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1","extra":{"nested":[{"$wbEncrypted":1,"envelope":"FFF"}]}}}`)
		_, err := Parse(raw)
		assertEncryptedEnvelopeRejected(t, err)
	})

	// 反向守卫：普通明文文件绝不能被误判为信封（否则所有账号都加载不了）。
	t.Run("plain auth still parses", func(t *testing.T) {
		raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1","nickname":"n1"}}`)
		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("plain auth must parse, got: %v", err)
		}
		if a.AccessToken != "at" || a.Nickname != "n1" {
			t.Errorf("parsed = %+v, want at/n1", a)
		}
	})
}
