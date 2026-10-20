package upstream

import "testing"

func TestClassifyModelEntitlement(t *testing.T) {
	cases := []string{
		`{"error":{"data":{"code":11158,"msg":"model not allowed for user type"}}}`,
		`{"code":11158,"msg":"model not allowed for user type"}`,
	}
	for _, body := range cases {
		if got := Classify(403, body); got != ErrModelEntitlement {
			t.Errorf("Classify(403,%s)=%v want ErrModelEntitlement", body, got)
		}
	}
	// 无 11158 的普通 4xx 不应误判。
	if got := Classify(400, `{"code":11101,"msg":"Unmarshal chat params failed"}`); got == ErrModelEntitlement {
		t.Errorf("11101 misclassified as entitlement")
	}
}
