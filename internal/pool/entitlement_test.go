package pool

import "testing"

func TestModelEntitlementLedger(t *testing.T) {
	p := New("")
	p.NoteModelEntitlement("cn", "glm-5.2", "11158")
	es := p.ModelEntitlements()
	if len(es) != 1 || es[0].Realm != "cn" || es[0].Model != "glm-5.2" || es[0].Code != "11158" {
		t.Fatalf("entitlements=%+v", es)
	}
	p.ClearModelEntitlement("cn", "glm-5.2")
	if got := p.ModelEntitlements(); len(got) != 0 {
		t.Fatalf("clear failed: %+v", got)
	}
}
