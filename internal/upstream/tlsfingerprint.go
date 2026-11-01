// tlsfingerprint.go 出站 TLS ClientHello 指纹复刻（Node 22 / OpenSSL 3.x）。
//
// 请求头做到 100% 一致也只能骗过 HTTP 层——TLS 握手在 HTTP 之前就完成了。
// Go 的 crypto/tls 有自己一套固定形态（13 个 cipher、混入后量子曲线、带
// SCT(18) 与 status_request(5)），与官方客户端的 Node 栈完全不同。
//
// 真机证据（2026-11-01 抓包，MITM 记录完整 ClientHello 原始字节）：
//
//	国内 www.workbuddy.cn   JA3 71dc8c533dd919ae9f4963224a4ba8fd
//	国际 www.workbuddy.ai   JA3 71dc8c533dd919ae9f4963224a4ba8fd
//	                        JA4 t13d0a1200_5d04281c6031_b76c5ee2507d
//
// 两版完全同值——一套 spec 覆盖 CN 与 GL。官方 chat 走 Node 主进程而非渲染器
// Chromium，所以 ClientHello **不带 ALPN**，与本仓「禁 h2」策略天然一致。
package upstream

import (
	"crypto/tls"
	"fmt"
	"strings"
	"sync/atomic"

	utls "github.com/refraction-networking/utls"
)

// TLS 指纹档位。
const (
	// TLSFingerprintNode 复刻官方 Node/OpenSSL 形态。
	TLSFingerprintNode = "node"
	// TLSFingerprintGo 用 crypto/tls 原生形态（排障/回滚用，也是本仓默认）。
	TLSFingerprintGo = "go"
)

// 真机实测常量（抓包取得，用于回归断言，不是推算值）。
const (
	// OfficialJA3MD5 官方 chat 链路 ClientHello 的 JA3 md5。
	OfficialJA3MD5 = "71dc8c533dd919ae9f4963224a4ba8fd"
	// OfficialJA3 官方 chat 链路 ClientHello 的 JA3 原文。
	OfficialJA3 = "771," +
		"4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53," +
		"0-23-65281-10-11-35-13-51-45-43," +
		"29-23-24," +
		"0"
	// OfficialJA4 官方 chat 链路的 JA4。
	OfficialJA4 = "t13d0a1200_5d04281c6031_b76c5ee2507d"
)

// officialCipherSuites 官方 ClientHello 的 18 个 cipher，顺序即真机顺序。
// 与 Go 默认的差异不只是数量：TLS1.3 三个套件排最前，Go 排在末尾。
var officialCipherSuites = []uint16{
	tls.TLS_AES_128_GCM_SHA256,
	tls.TLS_AES_256_GCM_SHA384,
	tls.TLS_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
}

// officialSigAlgs 官方 signature_algorithms 的 9 项。
// JA3 看不到这组值，但 JA4 第三段完全由它决定——少一个（ed25519/ed448/
// pss_pss 官方都不发）或多一个，JA4 立刻跑偏。
var officialSigAlgs = []utls.SignatureScheme{
	0x0403, // ecdsa_secp256r1_sha256
	0x0804, // rsa_pss_rsae_sha256
	0x0401, // rsa_pkcs1_sha256
	0x0503, // ecdsa_secp384r1_sha384
	0x0805, // rsa_pss_rsae_sha384
	0x0501, // rsa_pkcs1_sha384
	0x0806, // rsa_pss_rsae_sha512
	0x0601, // rsa_pkcs1_sha512
	0x0201, // rsa_pkcs1_sha1
}

// officialHelloSpec 复刻官方 Node/OpenSSL ClientHello。
// 扩展顺序即真机顺序：SNI → EMS → renegotiation → groups → ecpf →
// session_ticket → sigalgs → key_share → psk_modes → supported_versions。
func officialHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CipherSuites:       officialCipherSuites,
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.UtlsExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,
				utls.CurveP256,
				utls.CurveP384,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: officialSigAlgs},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519},
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}},
		},
	}
}

// tlsFingerprintValue 当前指纹档位（默认 go = 保持改前行为；node 需显式开启）。
var tlsFingerprintValue atomic.Value // string

func init() { tlsFingerprintValue.Store(TLSFingerprintGo) }

// SetTLSFingerprint 切换出站 ClientHello 指纹档位（"node" / "go"）。
// 运行期可切：出现「换指纹反而握手失败」时改配置即可回滚。
func SetTLSFingerprint(fp string) error {
	norm, err := validateTLSFingerprint(fp)
	if err != nil {
		return err
	}
	tlsFingerprintValue.Store(norm)
	return nil
}

// currentTLSFingerprint 当前档位（空值按 go 处理）。
func currentTLSFingerprint() string {
	if v, ok := tlsFingerprintValue.Load().(string); ok {
		return v
	}
	return TLSFingerprintGo
}

// useNodeFingerprint 该次拨号是否走复刻指纹。
func useNodeFingerprint(fp string) bool {
	return strings.EqualFold(strings.TrimSpace(fp), TLSFingerprintNode)
}

// validateTLSFingerprint 校验配置取值，返回规范化后的值（空 = go）。
func validateTLSFingerprint(fp string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(fp)); v {
	case "", TLSFingerprintGo:
		return TLSFingerprintGo, nil
	case TLSFingerprintNode:
		return TLSFingerprintNode, nil
	default:
		return "", fmt.Errorf("upstream.tls_fingerprint: unsupported %q (want node or go)", fp)
	}
}
