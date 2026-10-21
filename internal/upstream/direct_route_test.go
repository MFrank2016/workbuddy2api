package upstream

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestDirectRouteBypassesEnvProxy 账号级强制直连（issue #223 / 面板 0013）：
// proxy=__direct__ 必须绕过进程级环境代理，且与普通线路分开连接池。
func TestDirectRouteBypassesEnvProxy(t *testing.T) {
	ConfigureProxies(map[string]string{
		DirectRouteName: "http://should-be-ignored:8080",
		"route-a":       "http://a.example:8080",
	})
	t.Cleanup(func() { ConfigureProxies(nil) })

	tr, err := TransportFor(DirectRouteName, "")
	if err != nil {
		t.Fatalf("直连线路不应报错：%v", err)
	}
	if tr.Proxy != nil {
		t.Fatal("直连 Transport 的 Proxy 必须为 nil，否则会继续走环境代理")
	}
	again, _ := TransportFor(DirectRouteName, "")
	if again != tr {
		t.Error("直连 Transport 没有复用同一实例（连接池会分裂）")
	}
	route, _ := TransportFor("route-a", "")
	if route == tr {
		t.Error("直连与普通线路共用了 Transport —— 连接池会串")
	}

	c := New()
	client, err := c.clientFor(&auth.Auth{UID: "u-direct", ProxyName: DirectRouteName}, false)
	if err != nil {
		t.Fatalf("clientFor 直连失败：%v", err)
	}
	got, ok := client.Transport.(*http.Transport)
	if !ok || got.Proxy != nil {
		t.Fatalf("账号直连没有落到无代理 Transport：%#v", client.Transport)
	}
}

// TestRouteConfigErrorIsTyped 线路配置错误必须是 *Error / ErrRouteConfig。
//
// 否则 handler 会把它当成「网络抖动」：NoteFailures + fail + 换号，一次拼错
// 就能把整池打空并最终 503 —— 而这是配置问题，换多少个号都一样。
func TestRouteConfigErrorIsTyped(t *testing.T) {
	ConfigureProxies(map[string]string{"known": "http://p.example:8080"})
	t.Cleanup(func() { ConfigureProxies(nil) })

	c := New()
	_, err := c.clientFor(&auth.Auth{UID: "u1", ProxyName: "no-such-route"}, false)
	if err == nil {
		t.Fatal("未知线路应报错")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("应为 *upstream.Error，实际 %T", err)
	}
	if ue.Kind != ErrRouteConfig {
		t.Fatalf("Kind=%v want ErrRouteConfig", ue.Kind)
	}
}

// TestSessionNameIsSanitized 会话名不能把代理地址改成别的主机。
//
// `proxy_session` 来自 auth 文件（可能手写），含 @ / / / : 等字符时，替换进
// `http://{account}:pass@gw:8080` 会把主机解析成攻击者指定的地址。
func TestSessionNameIsSanitized(t *testing.T) {
	ConfigureProxies(map[string]string{
		"tmpl": "http://{account}:secret@gateway:8080",
	})
	t.Cleanup(func() { ConfigureProxies(nil) })

	tr, err := TransportFor("tmpl", "evil.com/@:x y")
	if err != nil {
		t.Fatalf("TransportFor: %v", err)
	}
	u, err := tr.Proxy(nil)
	if err != nil || u == nil {
		t.Fatalf("Proxy(): %v", err)
	}
	if u.Host != "gateway:8080" {
		t.Fatalf("会话名把主机改掉了：host=%q（应仍为 gateway:8080）", u.Host)
	}
	if strings.Contains(u.User.Username(), "@") || strings.Contains(u.User.Username(), "/") {
		t.Fatalf("会话名未转义：%q", u.User.Username())
	}
}

// TestProxyErrorDoesNotLeakCredentials 代理地址错误不能回显 user:password。
func TestProxyErrorDoesNotLeakCredentials(t *testing.T) {
	ConfigureProxies(map[string]string{
		// 端口非法 → url.Parse 失败；地址里带凭据（*url.Error 会回显原文）。
		"bad": "http://user:sup3rsecret@host:not-a-port",
	})
	t.Cleanup(func() { ConfigureProxies(nil) })

	_, err := TransportFor("bad", "")
	if err == nil {
		t.Fatal("应报错")
	}
	if strings.Contains(err.Error(), "sup3rsecret") {
		t.Fatalf("错误文本泄露了代理密码：%v", err)
	}
}
