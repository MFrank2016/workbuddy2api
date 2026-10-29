package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

func newPlanTestClient(t *testing.T, payload string) (*Client, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:              srv.Client(),
		BillingBaseCN:     srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}, &auth.Auth{UID: "u1", AccessToken: "at", Domain: "copilot.tencent.com"}
}

func TestIdentifyPlanCN(t *testing.T) {
	payload := `{"code":0,"data":{"Response":{"Data":{"Accounts":[{` +
		`"PackageName":"高级版","SubscriptionPackageCode":"p_advanced","Status":0,` +
		`"StartTime":"2026-01-01 00:00:00","EndTime":"2027-01-01 00:00:00",` +
		`"CapacitySize":1000,"CapacityRemain":500}]}}}}`
	c, a := newPlanTestClient(t, payload)
	got, err := c.IdentifyPlan(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "高级版" || !got.Paid || got.Trial {
		t.Fatalf("got=%+v", got)
	}
	if got.Source != "package_code" {
		t.Fatalf("source=%q", got.Source)
	}
}

func TestIdentifyPlanIntlTrial(t *testing.T) {
	payload := `{"code":0,"data":{"Response":{"Data":{"Accounts":[{` +
		`"PackageName":"Pro试用","SubscriptionPackageCode":"pro_trial","Status":0,` +
		`"EndTime":"2027-01-01 00:00:00","CapacitySize":100,"CapacityRemain":50}]}}}}`
	c, a := newPlanTestClient(t, payload)
	a.Domain = "www.workbuddy.ai"
	got, err := c.IdentifyPlan(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "Pro试用" || !got.Trial {
		t.Fatalf("got=%+v", got)
	}
}

func TestIdentifyPlanEnterprise(t *testing.T) {
	c, a := newPlanTestClient(t, `{"code":0,"data":{}}`)
	a.EnterpriseID = "ent-1"
	got, err := c.IdentifyPlan(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "企业版" || !got.Enterprise {
		t.Fatalf("got=%+v", got)
	}
}

func TestIdentifyPlanExpiredStaysPending(t *testing.T) {
	payload := `{"code":0,"data":{"Response":{"Data":{"Accounts":[{` +
		`"PackageName":"旗舰版","Status":0,"EndTime":"2020-01-01 00:00:00"}]}}}}`
	c, a := newPlanTestClient(t, payload)
	got, err := c.IdentifyPlan(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "待确认" {
		t.Fatalf("expired plan should stay pending: %+v", got)
	}
}
