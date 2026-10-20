package panel

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testPasswordHash 生成低迭代摘要（测试快；VerifyPassword 按摘要内 iter 解析）。
func testPasswordHash(pw string, iter int) string {
	salt := []byte("0123456789abcdef")
	sum := pbkdf2SHA256([]byte(pw), salt, iter, 32)
	return "pbkdf2-sha256$" + strconv.Itoa(iter) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(sum)
}

func TestPasswordHashVerify(t *testing.T) {
	h := HashPassword("s3cret")
	if h == "" {
		t.Fatal("hash empty")
	}
	if !VerifyPassword(h, "s3cret") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword("garbage", "x") {
		t.Fatal("malformed hash accepted")
	}
}

func TestSessionSignVerify(t *testing.T) {
	h := testPasswordHash("pw", 1000)
	tok, exp := signSession(h, time.Hour)
	if !exp.After(time.Now()) {
		t.Fatal("exp in past")
	}
	if !verifySession(h, tok, time.Now()) {
		t.Fatal("valid token rejected")
	}
	if verifySession(h, tok, exp.Add(time.Second)) {
		t.Fatal("expired token accepted")
	}
	if verifySession(h, tok+"x", time.Now()) {
		t.Fatal("tampered token accepted")
	}
}

func TestWithAuthPasswordSession(t *testing.T) {
	h := testPasswordHash("pw", 1000)
	p := &Panel{cfg: Config{APIKey: "k", PasswordHash: h, SessionTTL: time.Hour}}
	ok := p.withAuth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// 无凭证 → 401
	rec := httptest.NewRecorder()
	ok(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cred code=%d", rec.Code)
	}

	// 会话 cookie → 200
	tok, _ := signSession(h, time.Hour)
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	rec = httptest.NewRecorder()
	ok(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie code=%d body=%s", rec.Code, rec.Body)
	}

	// 跨源 POST → 403（CSRF）
	req = httptest.NewRequest("POST", "/panel/api/config", strings.NewReader("{}"))
	req.Host = "gw.local"
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	rec = httptest.NewRecorder()
	ok(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("csrf code=%d", rec.Code)
	}

	// 同源 POST → 200
	req = httptest.NewRequest("POST", "/panel/api/config", strings.NewReader("{}"))
	req.Host = "gw.local"
	req.Header.Set("Origin", "http://gw.local")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	rec = httptest.NewRecorder()
	ok(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin code=%d", rec.Code)
	}
}

func TestPasswordLoginFlow(t *testing.T) {
	h := testPasswordHash("pw", 1000)
	p := New(Config{APIKey: "k", PasswordHash: h, SessionTTL: time.Hour})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/auth_mode", nil))
	if !strings.Contains(rec.Body.String(), `"password":true`) {
		t.Fatalf("auth_mode=%s", rec.Body)
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/login", strings.NewReader(`{"password":"nope"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/login", strings.NewReader(`{"password":"pw"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login code=%d body=%s", rec.Code, rec.Body)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != sessionCookieName || !cookies[0].HttpOnly {
		t.Fatalf("cookie=%+v", cookies)
	}
}

func TestPasswordLoginRateLimit(t *testing.T) {
	h := testPasswordHash("pw", 1000)
	p := New(Config{APIKey: "k", PasswordHash: h, SessionTTL: time.Hour})
	for i := 0; i < loginFailMax; i++ {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/login", strings.NewReader(`{"password":"bad"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d code=%d", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/login", strings.NewReader(`{"password":"bad"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after limit code=%d", rec.Code)
	}
}
