package logquery

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// textDecoder converts raw log bytes to UTF-8. Invalid input is replaced
// instead of aborting a query, and the replacement is reported to the caller.
type textDecoder struct {
	encoding string
}

func newTextDecoder(encoding string) textDecoder {
	normalized := strings.ToUpper(strings.TrimSpace(encoding))
	switch normalized {
	case "UTF8", "UTF-8", "":
		normalized = "UTF-8"
	case "GB2312":
		normalized = "GBK"
	case "GBK", "GB18030":
	default:
		normalized = "UTF-8"
	}
	return textDecoder{encoding: normalized}
}

// Decode returns UTF-8 text and whether invalid input had to be replaced.
func (d textDecoder) Decode(raw []byte) (string, bool) {
	raw = bytes.TrimPrefix(raw, utf8BOM)
	switch d.encoding {
	case "GBK":
		return decodeWith(raw, simplifiedchinese.GBK.NewDecoder())
	case "GB18030":
		return decodeWith(raw, simplifiedchinese.GB18030.NewDecoder())
	default:
		if utf8.Valid(raw) {
			return string(raw), false
		}
		return strings.ToValidUTF8(string(raw), "\uFFFD"), true
	}
}

func decodeWith(raw []byte, dec transform.Transformer) (string, bool) {
	out, n, err := transform.Bytes(dec, raw)
	if err == nil {
		return string(out), false
	}
	// Keep whatever was decoded and mark the remainder, rather than dropping
	// the whole line. This mirrors "safe replacement" behavior.
	rest := raw
	if n > 0 && n <= len(raw) {
		rest = raw[n:]
	}
	var b strings.Builder
	b.Write(out)
	if len(rest) > 0 {
		b.WriteRune('\uFFFD')
	}
	return b.String(), true
}
