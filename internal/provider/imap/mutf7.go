package imap

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Mailbox names (and Gmail labels) travel in modified UTF-7 (RFC 3501
// 5.1.3): printable ASCII as is, "&" as "&-", anything else as "&"
// + modified base64 of UTF-16 + "-".

var b64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

func encodeMUTF7(s string) string {
	var b strings.Builder
	var pending []rune
	flush := func() {
		if len(pending) == 0 {
			return
		}
		u := utf16.Encode(pending)
		buf := make([]byte, 0, len(u)*2)
		for _, x := range u {
			buf = append(buf, byte(x>>8), byte(x))
		}
		b.WriteByte('&')
		b.WriteString(b64.EncodeToString(buf))
		b.WriteByte('-')
		pending = pending[:0]
	}
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e {
			flush()
			if r == '&' {
				b.WriteString("&-")
			} else {
				b.WriteRune(r)
			}
			continue
		}
		pending = append(pending, r)
	}
	flush()
	return b.String()
}

// decodeMUTF7 decodes a name; input that is not valid modified UTF-7 is
// returned unchanged (some servers send UTF-8).
func decodeMUTF7(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}
		j := strings.IndexByte(s[i+1:], '-')
		if j < 0 {
			return s
		}
		enc := s[i+1 : i+1+j]
		i += j + 1
		if enc == "" {
			b.WriteByte('&')
			continue
		}
		buf, err := b64.DecodeString(enc)
		if err != nil || len(buf)%2 != 0 {
			return s
		}
		u := make([]uint16, len(buf)/2)
		for k := range u {
			u[k] = uint16(buf[2*k])<<8 | uint16(buf[2*k+1])
		}
		for _, r := range utf16.Decode(u) {
			if r == utf8.RuneError {
				return s
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
