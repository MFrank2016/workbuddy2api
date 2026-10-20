package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "wb_panel_session"
	pbkdf2Iterations  = 120000
	loginFailWindow   = 5 * time.Minute
	loginFailMax      = 10
)

// HashPassword 生成 PBKDF2-HMAC-SHA256 摘要：
// "pbkdf2-sha256$<iter>$<salt-b64>$<hash-b64>"。用于 panel.password_hash。
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return ""
	}
	sum := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iterations, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum))
}

// VerifyPassword 常量时间校验密码（格式非法一律 false）。
func VerifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 5_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2SHA256 是 PBKDF2-HMAC-SHA256（RFC 8018）内联实现：Go 1.22 标准库没有
// crypto/pbkdf2（Go 1.24 才加入），内联可避免为面板密码新增第三方依赖。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		_, _ = prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		_, _ = prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			_, _ = prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// signSession 用密码摘要派生 HMAC key 签发会话令牌：<expUnix>.<nonce>.<sig>。
func signSession(encoded string, ttl time.Duration) (string, time.Time) {
	exp := time.Now().Add(ttl)
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	payload := strconv.FormatInt(exp.Unix(), 10) + "." + hex.EncodeToString(nonce)
	mac := hmac.New(sha256.New, sessionKey(encoded))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), exp
}

// verifySession 校验令牌签名与过期时间。
func verifySession(encoded, token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	expUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || now.Unix() >= expUnix {
		return false
	}
	mac := hmac.New(sha256.New, sessionKey(encoded))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(parts[2]))
}

func sessionKey(encoded string) []byte {
	sum := sha256.Sum256([]byte("wb2api-panel-session\x00" + encoded))
	return sum[:]
}

type loginFail struct {
	n     int
	until time.Time
}

// allowLogin 每 IP 登录尝试限速（窗口内失败次数达上限则拒绝）。
func (p *Panel) allowLogin(ip string) bool {
	now := time.Now()
	p.pwdMu.Lock()
	defer p.pwdMu.Unlock()
	if p.pwdFails == nil {
		p.pwdFails = map[string]*loginFail{}
	}
	f := p.pwdFails[ip]
	if f == nil || now.After(f.until) {
		p.pwdFails[ip] = &loginFail{until: now.Add(loginFailWindow)}
		return true
	}
	return f.n < loginFailMax
}

// noteLoginFail 记录一次失败（窗口内累计）。
func (p *Panel) noteLoginFail(ip string) {
	now := time.Now()
	p.pwdMu.Lock()
	defer p.pwdMu.Unlock()
	if p.pwdFails == nil {
		p.pwdFails = map[string]*loginFail{}
	}
	f := p.pwdFails[ip]
	if f == nil || now.After(f.until) {
		f = &loginFail{until: now.Add(loginFailWindow)}
		p.pwdFails[ip] = f
	}
	f.n++
}

// remoteIP 取连接对端 IP（不信任 X-Forwarded-For：限速只防在线爆破，用真实对端）。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
