// official_retry_test.go official profile 的同号重试：gzip 只压一次，每次重试复用
// 同一份压缩字节，Content-Encoding / ContentLength 在每次请求上都正确。
package upstream

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestOfficialRetryReusesGzip(t *testing.T) {
	var bodies [][]byte
	var encodings []string
	var lengths []int64
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read outbound body: %v", err)
			}
			bodies = append(bodies, raw)
			encodings = append(encodings, r.Header.Get("Content-Encoding"))
			lengths = append(lengths, r.ContentLength)
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 500,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":500}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN: "https://chat.example",
		Retry:      RetryConfig{Enabled: true, Attempts: 1, Backoff: time.Millisecond},
	}
	a := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "copilot.tencent.com"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{RequestProfile: ProfileOfficial})
	if err != nil || status != 200 {
		t.Fatalf("ChatStream: status=%d err=%v", status, err)
	}
	rc.Close()
	if len(bodies) != 2 {
		t.Fatalf("attempts=%d want 2", len(bodies))
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("retry must reuse the same compressed bytes")
	}
	for i := range bodies {
		if encodings[i] != "gzip" {
			t.Fatalf("attempt %d Content-Encoding=%q want gzip", i+1, encodings[i])
		}
		if lengths[i] != int64(len(bodies[i])) {
			t.Fatalf("attempt %d ContentLength=%d body len=%d", i+1, lengths[i], len(bodies[i]))
		}
		zr, err := gzip.NewReader(bytes.NewReader(bodies[i]))
		if err != nil {
			t.Fatalf("attempt %d gzip: %v", i+1, err)
		}
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			t.Fatalf("attempt %d read gzip: %v", i+1, err)
		}
		if !strings.Contains(string(raw), `"stream_options":{"include_usage":true}`) {
			t.Fatalf("attempt %d missing official body defaults: %s", i+1, raw)
		}
		if !strings.Contains(string(raw), `"max_tokens":128000`) {
			t.Fatalf("attempt %d missing official model defaults: %s", i+1, raw)
		}
	}
}
