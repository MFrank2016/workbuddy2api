package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 到期快照的批次渲染：有 credits_expiry_buckets 时列出后续批次，缺失/单批时
// 退回「仅最早一批」提示，不能因为字段缺失抛错把整个首页卡片打空。
func TestAppJSExpiryBucketsRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs=require('fs'),vm=require('vm');
const src=fs.readFileSync(process.argv[2],'utf8');
const s=src.indexOf('function renderExpirySnapshot');
const e=src.indexOf('async function loadExpiry');
if(s<0||e<0) throw new Error('renderExpirySnapshot not found');
const nodes={};
const ctx={
  esc:s=>String(s),
  fmtTok:n=>String(n),
  $:id=>nodes[id]||(nodes[id]={innerHTML:'',textContent:'',hidden:true,children:[]}),
  isNaN, Date, Math, Number, String, Array, JSON,
};
vm.createContext(ctx);
vm.runInContext(src.slice(s,e)+'\nthis.renderExpirySnapshot=renderExpirySnapshot;',ctx);
const iso=(d)=>new Date(Date.now()+d*86400000).toISOString();
const base={
  uid:'u1',nickname:'acct',credits_earliest_expiry:iso(2),credits_earliest_remaining:300,
};
ctx.renderExpirySnapshot({accounts:[Object.assign({},base,{
  credits_expiry_buckets:[
    {date:iso(2).slice(0,10),expires_at:iso(2),remain:300},
    {date:iso(9).slice(0,10),expires_at:iso(9),remain:700},
  ],
})]});
const withBuckets=nodes['expList'].innerHTML;
ctx.renderExpirySnapshot({accounts:[Object.assign({},base,{credits_expiry_buckets:null})]});
const without=nodes['expList'].innerHTML;
process.stdout.write(JSON.stringify({
  hasBatches: withBuckets.includes('后续批次'),
  hasSecond: withBuckets.includes('700'),
  fallback: without.includes('仅最早一批'),
  noCrash: without.length>0,
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-buckets-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry buckets node test failed: %v\n%s", err, out)
	}
	const want = `{"hasBatches":true,"hasSecond":true,"fallback":true,"noCrash":true}`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("expiry buckets render = %s, want %s", got, want)
	}
}
