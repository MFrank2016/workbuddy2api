package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Global 模型可用性标签：模型锁优先于最近请求；无本地证据时保持「待验证」，
// 不能把上游目录里出现模型误判为当前账号有权限。
//
// 这是纯前端逻辑（app.js 的 mdAvailabilityOf），只能靠 node 跑真实代码验证：
// Go 侧编译不检查 JS，标签写错只会让运维把「上游列了模型」当成「现在能用」。
func TestAppJSModelAvailability(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model availability test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdAvailabilityOf');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model availability helpers not found');
const ctx = { esc: s => String(s) };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdAvailabilityOf=mdAvailabilityOf; this.mdAvailabilityTag=mdAvailabilityTag;', ctx);
const model = id => ({ id });
const state = (m, locks, recent) => ctx.mdAvailabilityOf(m, locks, recent).key;
const success = [{ model: 'global:ok', status: 200, outcome: 'success', time: '2026-10-18T12:00:00Z' }];
ctx.mdLocks = [{ realm: 'global', model: 'gpt-5.6-sol', state: 'locked', reason: '11158 model not allowed for account type' }];
ctx.mdRecent = [];
const deniedTag = ctx.mdAvailabilityTag(model('global:gpt-5.6-sol'));
process.stdout.write(JSON.stringify({
  cn: ctx.mdAvailabilityOf(model('cn:glm-5.3'), [], []),
  unknown: state(model('global:unknown'), [], []),
  denied: state(model('global:gpt-5.6-sol'), [{ realm: 'global', model: 'gpt-5.6-sol', state: 'locked', reason: '11158 model not allowed for account type' }], []),
  missing: state(model('global:auto-chat'), [{ realm: 'global', model: 'auto-chat', state: 'locked', reason: '11102 service info not found' }], []),
  partial: state(model('global:glm-5.3'), [{ realm: 'global', model: 'glm-5.3', state: 'partial', reason: '6004 model rate limit' }], []),
  starved: state(model('global:glm-5.3'), [{ realm: 'global', model: 'glm-5.3', state: 'starved' }], []),
  rate: state(model('global:glm-5.3'), [], [{ model: 'global:glm-5.3', status: 429 }]),
  upstream: state(model('global:glm-5.3'), [], [{ model: 'global:glm-5.3', status: 503 }]),
  ok: state(model('global:ok'), [], success),
  deniedTag: deniedTag.includes('无权限') && deniedTag.includes('tag bad'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-availability-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model availability node test failed: %v\n%s", err, out)
	}
	const want = `{"cn":null,"unknown":"unknown","denied":"denied","missing":"missing","partial":"partial","starved":"starved","rate":"rate","upstream":"upstream","ok":"ok","deniedTag":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model availability=%s want %s", out, want)
	}
}
