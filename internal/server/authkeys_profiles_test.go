package server

import (
	"reflect"
	"testing"
)

// TestAuthKeyProfilesCopy matchAuthKey 必须深拷贝 Profiles（与 Groups 同理）。
func TestAuthKeyProfilesCopy(t *testing.T) {
	keys := []AuthKey{
		{Key: "sk-a", Name: "A", Profiles: []string{"cn-cli"}},
	}
	got := matchAuthKey(keys, "Bearer sk-a")
	if got == nil {
		t.Fatal("未命中")
	}
	if !reflect.DeepEqual(got.Profiles, []string{"cn-cli"}) {
		t.Fatalf("Profiles=%v want [cn-cli]", got.Profiles)
	}
	got.Profiles[0] = "intl-cli"
	if keys[0].Profiles[0] != "cn-cli" {
		t.Error("matchAuthKey 返回了表内 Profiles 的引用（应为副本）")
	}
}
