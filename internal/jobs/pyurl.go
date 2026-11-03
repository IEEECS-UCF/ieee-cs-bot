package jobs

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

// URLParts is what urllib.parse.urlparse returns. Go's net/url differs (it
// rejects some URLs and keeps ;params in the path), so links are split the
// way Python did when they're compared.
type URLParts struct{ Scheme, Netloc, Path, Params, Query, Fragment string }

var usesParams = map[string]bool{
	"": true, "ftp": true, "hdl": true, "prospero": true, "http": true, "imap": true, "https": true, "shttp": true,
	"rtsp": true, "rtsps": true, "rtspu": true, "sip": true, "sips": true, "mms": true, "sftp": true, "tel": true,
}

func isSchemeChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}

// URLParse is urllib.parse.urlparse.
func URLParse(raw string) URLParts {
	raw = strings.TrimLeft(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f"+
		"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	for _, c := range []string{"\t", "\r", "\n"} {
		raw = strings.ReplaceAll(raw, c, "")
	}
	var p URLParts
	if i := strings.IndexByte(raw, ':'); i > 0 && (raw[0] >= 'a' && raw[0] <= 'z' || raw[0] >= 'A' && raw[0] <= 'Z') {
		ok := true
		for k := 0; k < i; k++ {
			ok = ok && isSchemeChar(raw[k])
		}
		if ok {
			p.Scheme, raw = strings.ToLower(raw[:i]), raw[i+1:]
		}
	}
	if strings.HasPrefix(raw, "//") {
		end := len(raw)
		if i := strings.IndexAny(raw[2:], "/?#"); i >= 0 {
			end = i + 2
		}
		p.Netloc, raw = raw[2:end], raw[end:]
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw, p.Fragment = raw[:i], raw[i+1:]
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw, p.Query = raw[:i], raw[i+1:]
	}
	if usesParams[p.Scheme] && strings.Contains(raw, ";") {
		i := strings.IndexByte(raw, ';')
		if slash := strings.LastIndexByte(raw, '/'); slash >= 0 {
			i = strings.IndexByte(raw[slash:], ';')
			if i >= 0 {
				i += slash
			}
		}
		if i >= 0 {
			raw, p.Params = raw[:i], raw[i+1:]
		}
	}
	p.Path = raw
	return p
}

// ParseQSL is urllib.parse.parse_qsl: name/value pairs, without blank values.
func ParseQSL(qs string) [][2]string {
	var out [][2]string
	for _, pair := range strings.Split(qs, "&") {
		name, value, ok := strings.Cut(pair, "=")
		if pair == "" || !ok || value == "" {
			continue
		}
		out = append(out, [2]string{unquotePlus(name), unquotePlus(value)})
	}
	return out
}

// unquotePlus is unquote_plus: %XX escapes decode as UTF-8 (bad bytes become
// U+FFFD) and anything that isn't an escape is kept as it is.
func unquotePlus(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	var run []byte
	flush := func() {
		for len(run) > 0 {
			r, size := utf8.DecodeRune(run)
			b.WriteRune(r)
			run = run[size:]
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			run = append(run, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		flush()
		b.WriteByte(s[i])
	}
	flush()
	return b.String()
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// URLEncode is urllib.parse.urlencode of pairs (quote_plus on both sides).
func URLEncode(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1])
	}
	return strings.Join(parts, "&")
}
