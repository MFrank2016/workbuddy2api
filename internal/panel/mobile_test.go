package panel

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 手机端适配层必须作为同源静态资源可取到：index.html 用 <link>/<script src> 引用，
// 严格 CSP（style-src/script-src 'self'）会拦掉内联形式，取不到就整页错位。
func TestMobileAssetsServed(t *testing.T) {
	p := newTestPanel()
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/panel/mobile.css", "text/css"},
		{"/panel/mobile.js", "javascript"},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s code=%d want 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.want) {
			t.Errorf("%s Content-Type=%q want %s", tc.path, ct, tc.want)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s served an empty body", tc.path)
		}
	}
}

// index.html 必须挂上手机端资源，且顺序正确：mobile.css 在主 <style> 之后
// （@media 覆盖依赖层叠顺序），mobile.js 在 app.js 之后（依赖其全局函数）。
func TestMobileAssetsWired(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	css := strings.Index(body, `<link rel="stylesheet" href="mobile.css">`)
	style := strings.Index(body, "</style>")
	if css < 0 {
		t.Fatal("index.html 未引用 mobile.css")
	}
	if style < 0 || css < style {
		t.Error("mobile.css 必须在主 <style> 之后引用，否则 @media 覆盖不生效")
	}

	appJS := strings.Index(body, `<script src="app.js"></script>`)
	mobileJS := strings.Index(body, `<script src="mobile.js"></script>`)
	if mobileJS < 0 {
		t.Fatal("index.html 未引用 mobile.js")
	}
	if appJS < 0 || mobileJS < appJS {
		t.Error("mobile.js 必须在 app.js 之后加载，它依赖 app.js 的全局函数")
	}

	// 抽屉三件套：侧栏 id、汉堡按钮、遮罩，缺一个抽屉就开不了或关不掉。
	for _, marker := range []string{`id="navDrawer"`, `id="navBtn"`, `id="navVeil"`} {
		if !strings.Contains(body, marker) {
			t.Errorf("index.html 缺 %s", marker)
		}
	}
}

// mobile.js 必须能通过 JS 解析器校验，且不得引用我们 app.js 里不存在的全局，
// 否则手机端整页白屏（Go 编译不检查 go:embed 的 JS）。
func TestMobileJSSyntaxAndGlobals(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("mobile.js")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("mobile.js syntax error:\n%s", out)
	}

	// mobile.js 末尾会接管 renderUsageChart，被它调用的辅助函数必须真实存在于 app.js。
	appSrc, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	mobileSrc, err := os.ReadFile("mobile.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"parsePointTime", "fmtTok", "fmtTokTimeLabel", "esc", "renderUsageChart",
	} {
		if !strings.Contains(string(mobileSrc), name) {
			continue // 本版本没用到，不必要求 app.js 提供
		}
		if !strings.Contains(string(appSrc), name) {
			t.Errorf("mobile.js 使用 %s，但 app.js 未定义它", name)
		}
	}
}

// 表格卡片化的字段名必须来自表头纯文本，不能把排序箭头等装饰 <span> 的字符
// 也算进 data-l。这段逻辑只在浏览器里跑，用 node 直接验证 thLabel。
func TestMobileJSThLabel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs=require('fs'),vm=require('vm');
const src=fs.readFileSync(process.argv[2],'utf8');
const s=src.indexOf('function thLabel'),e=src.indexOf('/* labelTable');
const ctx={};vm.createContext(ctx);
vm.runInContext(src.slice(s,e)+'\nthis.thLabel=thLabel;',ctx);
const th={childNodes:[{nodeType:3,nodeValue:'  上下文'},{nodeType:1},{nodeType:3,nodeValue:'长度  '}]};
process.stdout.write(JSON.stringify(ctx.thLabel(th)));`
	f, err := os.CreateTemp(t.TempDir(), "thlabel-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "mobile.js").CombinedOutput()
	if err != nil {
		t.Fatalf("thLabel node test failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != `"上下文长度"` {
		t.Fatalf("thLabel=%s want \"上下文长度\" (装饰 span 不应计入)", got)
	}
}
