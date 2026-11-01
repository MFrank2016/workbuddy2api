package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"

	"workbuddy2api/internal/auth"
)

func testGzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testZlibBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testRawFlateBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := flate.NewWriter(&b, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testBrotliBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := brotli.NewWriter(&b)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestDecodeResponseBody 显式 Accept-Encoding 后的解压覆盖：无编码 / gzip /
// br / deflate(zlib) / deflate(裸 flate)。漏一种都会让对应 JSON 链路全废。
func TestDecodeResponseBody(t *testing.T) {
	plain := []byte(`{"code":0,"msg":"ok"}`)
	cases := []struct {
		name string
		enc  string
		body []byte
	}{
		{"none", "", plain},
		{"identity", "identity", plain},
		{"gzip", "gzip", testGzipBytes(t, plain)},
		{"br", "br", testBrotliBytes(t, plain)},
		{"deflate-zlib", "deflate", testZlibBytes(t, plain)},
		{"deflate-raw", "deflate", testRawFlateBytes(t, plain)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tc.enc != "" {
				resp.Header.Set("Content-Encoding", tc.enc)
			}
			got, err := decodeResponseBody(resp, tc.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("got %q want %q", got, plain)
			}
		})
	}
}

// TestDoJSONDecodesCompressedError 400 + br 的 body 必须先解压再 Classify：
// 不解压会把压缩字节当文本分类，硬积分耗尽会被误判成别的错误、罚错号。
func TestDoJSONDecodesCompressedError(t *testing.T) {
	const body = `{"code":1,"msg":"insufficient credits"}`
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Encoding": []string{"br"}},
			Body:       io.NopCloser(bytes.NewReader(testBrotliBytes(t, []byte(body)))),
		}, nil
	})}}
	req, err := http.NewRequest("POST", "https://fake.example/billing", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.doJSON(&auth.Auth{UID: "u1"}, req)
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err=%v want *upstream.Error", err)
	}
	if ue.Kind != ErrHardCredit {
		t.Fatalf("Kind=%v want ErrHardCredit（分类必须看到解压后的文本）", ue.Kind)
	}
}

// TestDoJSONSetsOfficialAcceptEncoding 请求必须显式声明官方四段编码。
func TestDoJSONSetsOfficialAcceptEncoding(t *testing.T) {
	var got string
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Get("Accept-Encoding")
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{}}`)),
		}, nil
	})}}
	req, _ := http.NewRequest("POST", "https://fake.example/x", strings.NewReader("{}"))
	if _, err := c.doJSON(&auth.Auth{UID: "u1"}, req); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if got != officialAcceptEncoding {
		t.Fatalf("Accept-Encoding=%q want %q", got, officialAcceptEncoding)
	}
}
