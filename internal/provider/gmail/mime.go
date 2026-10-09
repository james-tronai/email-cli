package gmail

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

// MaxMessageSize is Gmail's limit for a message including attachments
// (after base64 encoding) through the upload endpoint.
const MaxMessageSize = 35 << 20

// outgoing is a message to be written as RFC 822 for drafts.create/update.
type outgoing struct {
	From        provider.Address
	To, Cc, Bcc []provider.Address
	Subject     string
	Body        string // plain text
	InReplyTo   string
	References  string
	Attachments []provider.AttachmentFile
	Date        time.Time
}

// noCRLF removes line breaks so a value cannot inject headers.
func noCRLF(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

func formatAddrs(as []provider.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		if a.Email == "" {
			continue
		}
		ma := mail.Address{Name: noCRLF(a.Name), Address: noCRLF(a.Email)}
		parts = append(parts, ma.String())
	}
	return strings.Join(parts, ", ")
}

func randomBoundary() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "email-cli-" + hex.EncodeToString(b[:])
}

// build serialises the message. Text goes out as UTF-8 quoted-printable;
// with attachments the message is multipart/mixed.
func (o *outgoing) build() ([]byte, error) {
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
	hdr("From", formatAddrs([]provider.Address{o.From}))
	hdr("To", formatAddrs(o.To))
	hdr("Cc", formatAddrs(o.Cc))
	hdr("Bcc", formatAddrs(o.Bcc))
	hdr("Subject", mime.QEncoding.Encode("utf-8", noCRLF(o.Subject)))
	hdr("Date", date.Format(time.RFC1123Z))
	hdr("In-Reply-To", noCRLF(o.InReplyTo))
	hdr("References", noCRLF(o.References))
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
	if buf.Len() > MaxMessageSize {
		return nil, errs.Unsupported("the message is %d bytes; Gmail accepts at most %d", buf.Len(), MaxMessageSize)
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
	ctype := firstNonEmpty(a.MIME, "application/octet-stream")
	if _, _, err := mime.ParseMediaType(ctype); err != nil {
		ctype = "application/octet-stream"
	}
	name := noCRLF(a.Name)
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

// replySubject adds "Re: " unless the subject already has a reply prefix.
func replySubject(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(t), "re:") {
		return t
	}
	return "Re: " + t
}

func forwardSubject(s string) string {
	t := strings.TrimSpace(s)
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "fwd:") || strings.HasPrefix(low, "fw:") {
		return t
	}
	return "Fwd: " + t
}

// references appends msgID to the original References chain.
func references(orig, msgID string) string {
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

// quote prefixes each line with "> ".
func quote(text string) string {
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
