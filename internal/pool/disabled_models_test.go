package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestDisabledModelsExcludedFromSelection(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", DisabledModels: []string{"glm-5.2"}})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "b"})
	p.mu.RLock()
	e1 := p.byUID["u1"]
	p.mu.RUnlock()
	if e1.healthyForModel(time.Now(), "glm-5.2") {
		t.Fatal("禁用模型不应可用")
	}
	if !e1.healthyForModel(time.Now(), "deepseek-v4.1-flash") {
		t.Fatal("其他模型应可用")
	}
	st, _ := p.Status("u1")
	if len(st.DisabledModels) != 1 || st.DisabledModels[0] != "glm-5.2" {
		t.Fatalf("status disabled_models=%v", st.DisabledModels)
	}
}
