package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelDisabled(t *testing.T) {
	a := &Auth{DisabledModels: []string{"glm-5.2", "gpt-5.6-*"}}
	cases := []struct {
		m    string
		want bool
	}{
		{"glm-5.2", true},
		{"gpt-5.6-luna", true},
		{"gpt-5.6", false},
		{"other", false},
	}
	for _, c := range cases {
		if got := a.ModelDisabled(c.m); got != c.want {
			t.Errorf("ModelDisabled(%q)=%v want %v", c.m, got, c.want)
		}
	}
	if (&Auth{}).ModelDisabled("x") {
		t.Error("empty disabled list should not disable")
	}
}

func TestParseDisabledModelsAndSaveRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	raw := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":9999999999},"account":{"uid":"u1"},"disabled_models":["glm-5.2","gpt-5.6-*"]}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.DisabledModels) != 2 || !a.ModelDisabled("glm-5.2") || !a.ModelDisabled("gpt-5.6-x") {
		t.Fatalf("parsed disabled_models=%v", a.DisabledModels)
	}
	a.FilePath = fp
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := Parse(raw2)
	if err != nil {
		t.Fatal(err)
	}
	if len(a2.DisabledModels) != 2 {
		t.Fatalf("roundtrip lost disabled_models: %v", a2.DisabledModels)
	}
}
