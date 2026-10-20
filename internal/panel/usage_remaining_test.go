package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/usage"
)

func TestUsageRemaining(t *testing.T) {
	rec := usage.New("")
	now := time.Now()
	rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{TotalTokens: 1000, HasTotal: true}, true)
	rec.Add(now, "cn", "u2", "glm-5.2", usage.Delta{TotalTokens: 400, HasTotal: true}, true)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", Nickname: "A"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "b", Nickname: "B"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
	pn := New(Config{Pool: p, Usage: rec, APIKey: "k"})

	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/usage/remaining?hours=24", nil)
	req.Header.Set("Authorization", "Bearer k")
	pn.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}
	var d struct {
		Budgets []struct {
			Avg     int64 `json:"avg"`
			Samples int   `json:"samples"`
		} `json:"budgets"`
		Rows []struct {
			UID       string `json:"uid"`
			Used      int64  `json:"used"`
			Remaining *int64 `json:"remaining"`
			Cooling   bool   `json:"cooling"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &d); err != nil {
		t.Fatalf("json: %v body=%s", err, rec2.Body)
	}
	if len(d.Budgets) != 1 || d.Budgets[0].Avg != 1000 || d.Budgets[0].Samples != 1 {
		t.Fatalf("budgets=%+v", d.Budgets)
	}
	type row struct {
		used    int64
		rem     int64
		cooling bool
	}
	rows := map[string]row{}
	for _, r := range d.Rows {
		rem := int64(-1)
		if r.Remaining != nil {
			rem = *r.Remaining
		}
		rows[r.UID] = row{r.Used, rem, r.Cooling}
	}
	if u1 := rows["u1"]; u1.used != 1000 || u1.rem != 0 || !u1.cooling {
		t.Fatalf("u1=%+v", u1)
	}
	if u2 := rows["u2"]; u2.used != 400 || u2.rem != 600 || u2.cooling {
		t.Fatalf("u2=%+v", u2)
	}
}
