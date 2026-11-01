// decode_body.go 显式 Accept-Encoding 的配套解压。
//
// 一旦请求显式声明 Accept-Encoding，net/http 就**关闭透明解压**（Transport 只在
// 「自己加的 gzip」时才解）。不解这一层，doJSON 拿到 brotli/gzip 字节直接
// json.Unmarshal → 所有 JSON RPC 链路全废，而且 400 分支的 Classify 会把压缩体
// 当文本分类，罚号逻辑跟着错——两处是一个整体。
package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// officialAcceptEncoding 官方客户端全链路同值（真机 chat / report / billing /
// refresh 逐条统计）。`compress`(LZW) 官方虽声明但服务端从不回，收到时明确报错。
const officialAcceptEncoding = "gzip, compress, deflate, br"

// decodeResponseBody 按 Content-Encoding 解压响应体。
// 支持 gzip / deflate（zlib 包装与裸 flate）/ br；多段编码按逆序解。
func decodeResponseBody(resp *http.Response, raw []byte) ([]byte, error) {
	if resp == nil || len(raw) == 0 {
		return raw, nil
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return raw, nil
	}
	parts := strings.Split(enc, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		e := strings.TrimSpace(parts[i])
		var err error
		switch e {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			var zr *gzip.Reader
			zr, err = gzip.NewReader(bytes.NewReader(raw))
			if err == nil {
				raw, err = io.ReadAll(zr)
				_ = zr.Close()
			}
		case "deflate":
			raw, err = inflate(raw)
		case "br":
			raw, err = io.ReadAll(brotli.NewReader(bytes.NewReader(raw)))
		default:
			return nil, fmt.Errorf("unsupported content encoding %q", e)
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", e, err)
		}
	}
	return raw, nil
}

// inflate 解 deflate：RFC 7230 的实现可能是 zlib 包装或裸 flate，两种都试。
func inflate(raw []byte) ([]byte, error) {
	if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		out, rerr := io.ReadAll(zr)
		_ = zr.Close()
		if rerr == nil {
			return out, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(raw))
	defer fr.Close()
	return io.ReadAll(fr)
}
