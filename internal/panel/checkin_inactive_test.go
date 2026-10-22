// checkin_inactive_test.go 钉住面板单号签到的回执：活动未开启时 checkin_message
// 必须是「活动未开启」（而不是上游 400 报文），且**不得**把账号标成「今日已签」。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newCheckinPanel 构造带假上游的面板：daily-checkin 返回给定响应，余额查询返回 100。
func newCheckinPanel(t *testing.T, checkinStatus int, checkinBody string) (*Panel, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			if checkinStatus != 0 {
				w.WriteHeader(checkinStatus)
			}
			w.Write([]byte(checkinBody))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
				`{"CycleCapacitySize":500,"CycleCapacityRemain":100,"CycleCapacityUsed":400}]}}}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "n", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestPanelCheckinInactiveMessage 活动未开启 → checkin_message=活动未开启，
// checkin_done=false（账号今天没签到，不能显示「已签」）。
func TestPanelCheckinInactiveMessage(t *testing.T) {
	p, pl := newCheckinPanel(t, http.StatusBadRequest, `{"code":10001,"msg":"活动已结束，奖励已领取"}`)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/checkin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		OK             bool   `json:"ok"`
		CheckinDone    bool   `json:"checkin_done"`
		CheckinMessage string `json:"checkin_message"`
		Credits        *int64 `json:"credits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if resp.CheckinMessage != "活动未开启" {
		t.Errorf("checkin_message=%q want 活动未开启（不得回填 400 报文）", resp.CheckinMessage)
	}
	if resp.CheckinDone {
		t.Error("inactive 不得标成今日已签")
	}
	if st, _ := pl.Status("u1"); st.CheckinDone {
		t.Error("pool 的 checkin_done 不得被 inactive 置位")
	}
	// 余额照常刷新（活动状态与账号额度无关）。
	if resp.Credits == nil || *resp.Credits != 100 {
		t.Errorf("credits=%v want 100", resp.Credits)
	}
}

// TestPanelCheckinAlreadyStillMarksDone 「今天已签到」仍标已签（不能被 inactive 分支吞掉）。
func TestPanelCheckinAlreadyStillMarksDone(t *testing.T) {
	p, _ := newCheckinPanel(t, http.StatusBadRequest, `{"code":10001,"msg":"今天已签到"}`)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/panel/api/accounts/u1/checkin", nil))
	var resp struct {
		CheckinDone    bool   `json:"checkin_done"`
		CheckinMessage string `json:"checkin_message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if !resp.CheckinDone {
		t.Error("今天已签到应标 checkin_done")
	}
}
