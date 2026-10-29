package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type loginStubTransport struct{}

func (loginStubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	body := `{"code":0,"msg":"ok","data":{"state":"0123456789abcdef","authUrl":"https://www.codebuddy.cn/login?state=0123456789abcdef"}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    nil,
	}, nil
}

func TestLoginStartReturnsQR(t *testing.T) {
	old := loginHTTP
	loginHTTP = &http.Client{Transport: loginStubTransport{}}
	t.Cleanup(func() { loginHTTP = old })

	p := newTestPanel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"cn"}`))
	p.loginStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if url, _ := out["url"].(string); url == "" {
		t.Fatalf("missing url: %v", out)
	}
	if qr, _ := out["qr_png"].(string); !strings.HasPrefix(qr, "data:image/png;base64,") {
		t.Fatalf("missing qr_png: %v", out)
	}
}
