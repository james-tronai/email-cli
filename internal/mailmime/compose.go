// Package mailmime writes and reads RFC 5322 / MIME messages for the
// providers that deal in raw messages (gmail drafts, imap).
package mailmime

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

// Outgoing is a message to be written as RFC 822.
type Outgoing struct {
	From        provider.Address
	To, Cc, Bcc []provider.Address
	Subject     string
	Body        string // plain text
	InReplyTo   string
	References  string
	MessageID   string // "<…>"; empty: none (the server or the submit step adds one)
	Attachments []provider.AttachmentFile
	Date        time.Time
}

// NoCRLF removes line breaks so a value cannot inject headers.
func NoCRLF(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

func FormatAddrs(as []provider.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		if a.Email == "" {
			continue
		}
		ma := mail.Address{Name: NoCRLF(a.Name), Address: NoCRLF(a.Email)}
		parts = append(parts, ma.String())
	}
	return strings.Join(parts, ", ")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomBoundary() string { return "email-cli-" + randomHex(12) }

// NewMessageID returns a fresh "<random@domain>" Message-ID.
func NewMessageID(domain string) string {
	if domain == "" {
		domain = "email-cli.invalid"
	}
	return "<" + randomHex(16) + "@" + domain + ">"
}

// Build serialises the message. Text goes out as UTF-8 quoted-printable;
// with attachments the message is multipart/mixed. maxSize > 0 limits the
// result.
func (o *Outgoing) Build(maxSize int, server string) ([]byte, error) {
	var buf bytes.Buffer
	hdr := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&buf, "%s: %s\r\n", k, v)
		}
	}
	date := o.Date
	if date.IsZero() {
		date = time.Now()
	}
	hdr("From", FormatAddrs([]provider.Address{o.From}))
	hdr("To", FormatAddrs(o.To))
	hdr("Cc", FormatAddrs(o.Cc))
	hdr("Bcc", FormatAddrs(o.Bcc))
	hdr("Subject", mime.QEncoding.Encode("utf-8", NoCRLF(o.Subject)))
	hdr("Date", date.Format(time.RFC1123Z))
	hdr("Message-ID", NoCRLF(o.MessageID))
	hdr("In-Reply-To", NoCRLF(o.InReplyTo))
	hdr("References", NoCRLF(o.References))
	hdr("MIME-Version", "1.0")

	if len(o.Attachments) == 0 {
		writeTextPart(&buf, o.Body)
	} else {
		boundary := randomBoundary()
		hdr("Content-Type", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": boundary}))
		buf.WriteString("\r\n")
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		writeTextPart(&buf, o.Body)
		for _, a := range o.Attachments {
			fmt.Fprintf(&buf, "\r\n--%s\r\n", boundary)
			if err := writeAttachment(&buf, a); err != nil {
				return nil, err
			}
		}
		fmt.Fprintf(&buf, "\r\n--%s--\r\n", boundary)
	}
	if maxSize > 0 && buf.Len() > maxSize {
		return nil, errs.Unsupported("the message is %d bytes; %s accepts at most %d", buf.Len(), server, maxSize)
	}
	return buf.Bytes(), nil
}

func writeTextPart(buf *bytes.Buffer, body string) {
	buf.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	w := quotedprintable.NewWriter(buf)
	_, _ = w.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	_ = w.Close()
}

func writeAttachment(buf *bytes.Buffer, a provider.AttachmentFile) error {
	ctype := a.MIME
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(ctype); err != nil {
		ctype = "application/octet-stream"
	}
	name := NoCRLF(a.Name)
	fmt.Fprintf(buf, "Content-Type: %s\r\n", mime.FormatMediaType(ctype, map[string]string{"name": name}))
	fmt.Fprintf(buf, "Content-Disposition: %s\r\n", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	buf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString(a.Data)
	for len(enc) > 76 {
		buf.WriteString(enc[:76])
		buf.WriteString("\r\n")
		enc = enc[76:]
	}
	buf.WriteString(enc) // the CRLF before the next boundary belongs to the boundary
	return nil
}

// ReplySubject adds "Re: " unless the subject already has a reply prefix.
func ReplySubject(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(t), "re:") {
		return t
	}
	return "Re: " + t
}

func ForwardSubject(s string) string {
	t := strings.TrimSpace(s)
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "fwd:") || strings.HasPrefix(low, "fw:") {
		return t
	}
	return "Fwd: " + t
}

// References appends msgID to the original References chain.
func References(orig, msgID string) string {
	orig = strings.TrimSpace(orig)
	if msgID == "" {
		return orig
	}
	if orig == "" {
		return msgID
	}
	if strings.Contains(orig, msgID) {
		return orig
	}
	return orig + " " + msgID
}

// Quote prefixes each line with "> ".
func Quote(text string) string {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

// Dedupe drops empty addresses, the mailbox itself, and repeats.
func Dedupe(self string, lists ...[]provider.Address) []provider.Address {
	seen := map[string]bool{strings.ToLower(self): true}
	var out []provider.Address
	for _, l := range lists {
		for _, a := range l {
			k := strings.ToLower(a.Email)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

// Snippet collapses whitespace and keeps the first 200 characters.
func Snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
