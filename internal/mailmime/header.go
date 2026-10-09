package mailmime

import (
	"mime"
	"net/mail"
	"strings"

	"golang.org/x/net/html/charset"

	"github.com/james-tronai/email-cli/internal/provider"
)

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}

// DecodeHeader decodes RFC 2047 encoded words; undecodable input is
// returned as is.
func DecodeHeader(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

func ParseAddr(s string) provider.Address {
	s = strings.TrimSpace(s)
	if s == "" {
		return provider.Address{}
	}
	if a, err := (&mail.AddressParser{WordDecoder: wordDecoder}).Parse(s); err == nil {
		return provider.Address{Name: a.Name, Email: a.Address}
	}
	return provider.Address{Email: s}
}

func ParseAddrList(s string) []provider.Address {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	list, err := (&mail.AddressParser{WordDecoder: wordDecoder}).ParseList(s)
	if err != nil {
		// Malformed list: keep the raw text rather than dropping recipients.
		var out []provider.Address
		for _, x := range strings.Split(s, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, ParseAddr(x))
			}
		}
		return out
	}
	out := make([]provider.Address, len(list))
	for i, a := range list {
		out[i] = provider.Address{Name: a.Name, Email: a.Address}
	}
	return out
}

// HeaderAllow is what --headers returns.
var HeaderAllow = map[string]bool{
	"message-id": true, "in-reply-to": true, "references": true, "authentication-results": true,
	"return-path": true, "list-unsubscribe": true, "reply-to": true,
}
