package qr

import (
	"encoding/base64"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

func QRDataURI(text string, size int) (string, error) {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return "", err
	}
	png, err := q.PNG(size)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

func RenderTerminalQR(text string) (string, error) {
	q, err := qrcode.New(text, qrcode.Low)
	if err != nil {
		return "", err
	}
	bmp := q.Bitmap()
	var b strings.Builder
	for y := 0; y < len(bmp); y += 2 {
		for x := 0; x < len(bmp[y]); x++ {
			top := bmp[y][x]
			bottom := false
			if y+1 < len(bmp) {
				bottom = bmp[y+1][x]
			}
			switch {
			case top && bottom:
				b.WriteString("█")
			case top && !bottom:
				b.WriteString("▀")
			case !top && bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}
