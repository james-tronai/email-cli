package gmail

import (
	"bytes"
	"encoding/base64"
	"html"
	"io"
	"mime"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/james-tronai/email-cli/internal/provider"
)

// Web client links. /u/0/ is the first Google account signed in to the
// browser; Gmail switches accounts itself when that is another mailbox.
const webBase = "https://mail.google.com/mail/u/0/"

func messageLink(id string) string { return webBase + "#inbox/" + id }

// draftLink opens the draft in the compose window. Gmail's compose= takes
// the draft's message id, not the "r…" draft id.
func draftLink(messageID string) string { return webBase + "#drafts?compose=" + messageID }

type gHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gBody struct {
	AttachmentID string `json:"attachmentId"`
	Size         int64  `json:"size"`
	Data         string `json:"data"` // base64url
}

type gPart struct {
	PartID   string    `json:"partId"`
	MimeType string    `json:"mimeType"`
	Filename string    `json:"filename"`
	Headers  []gHeader `json:"headers"`
	Body     gBody     `json:"body"`
	Parts    []gPart   `json:"parts"`
}

type gMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	Snippet      string   `json:"snippet"`
	HistoryID    string   `json:"historyId"`
	InternalDate string   `json:"internalDate"` // ms since epoch, as a string
	Payload      *gPart   `json:"payload"`
}

type gDraft struct {
	ID      string    `json:"id"`
	Message *gMessage `json:"message"`
}

func (p *gPart) header(name string) string {
	if p == nil {
		return ""
	}
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func decodeData(s string) []byte {
	if s == "" {
		return nil
	}
	s = strings.TrimRight(s, "=")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		b, _ = base64.RawStdEncoding.DecodeString(s)
	}
	return b
}

// partText decodes a text part to UTF-8. Gmail returns the bytes after
// transfer decoding but in the part's own charset.
func partText(p *gPart) string {
	raw := decodeData(p.Body.Data)
	_, params, _ := mime.ParseMediaType(p.header("Content-Type"))
	cs := strings.ToLower(params["charset"])
	if cs == "" || cs == "utf-8" || cs == "us-ascii" || cs == "utf8" {
		return string(raw)
	}
	r, err := charset.NewReaderLabel(cs, bytes.NewReader(raw))
	if err != nil {
		return string(raw)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// isAttachment reports whether a part is a file rather than body text.
func isAttachment(p *gPart) bool {
	if p.Filename != "" {
		return true
	}
	disp, _, _ := mime.ParseMediaType(p.header("Content-Disposition"))
	return disp == "attachment"
}

func isInline(p *gPart) bool {
	disp, _, _ := mime.ParseMediaType(p.header("Content-Disposition"))
	if disp == "inline" {
		return true
	}
	return disp == "" && p.header("Content-Id") != ""
}

// walkParts visits every leaf part.
func walkParts(p *gPart, fn func(*gPart)) {
	if p == nil {
		return
	}
	if len(p.Parts) == 0 {
		fn(p)
		return
	}
	for i := range p.Parts {
		walkParts(&p.Parts[i], fn)
	}
}

// bodies returns the first text/plain and text/html body parts.
func bodies(payload *gPart) (text, htmlBody string) {
	var haveText, haveHTML bool
	walkParts(payload, func(p *gPart) {
		if isAttachment(p) {
			return
		}
		switch strings.ToLower(p.MimeType) {
		case "text/plain":
			if !haveText {
				text, haveText = partText(p), true
			}
		case "text/html":
			if !haveHTML {
				htmlBody, haveHTML = partText(p), true
			}
		}
	})
	return text, htmlBody
}

// attachments lists file parts. The id is the MIME part id ("1", "2.1"):
// Gmail's attachmentId changes on every fetch, the part id does not.
func attachments(payload *gPart) []provider.AttachmentMeta {
	var out []provider.AttachmentMeta
	walkParts(payload, func(p *gPart) {
		if !isAttachment(p) {
			return
		}
		out = append(out, provider.AttachmentMeta{ID: p.PartID, Name: p.Filename, Size: p.Body.Size,
			MIME: strings.ToLower(p.MimeType), Inline: isInline(p)})
	})
	return out
}

func findPart(payload *gPart, partID string) *gPart {
	var hit *gPart
	walkParts(payload, func(p *gPart) {
		if hit == nil && p.PartID == partID {
			hit = p
		}
	})
	return hit
}

func parseAddr(s string) provider.Address {
	s = strings.TrimSpace(s)
	if s == "" {
		return provider.Address{}
	}
	if a, err := (&mail.AddressParser{WordDecoder: wordDecoder}).Parse(s); err == nil {
		return provider.Address{Name: a.Name, Email: a.Address}
	}
	return provider.Address{Email: s}
}

func parseAddrList(s string) []provider.Address {
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
				out = append(out, parseAddr(x))
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

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}

func decodeHeader(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

// headerAllow is what --headers returns.
var headerAllow = map[string]bool{
	"message-id": true, "in-reply-to": true, "references": true, "authentication-results": true,
	"return-path": true, "list-unsubscribe": true, "reply-to": true,
}

// systemLabels maps Gmail system label ids to reserved names. Labels not
// listed here (UNREAD, STARRED, IMPORTANT, CATEGORY_*) become flags or are
// dropped.
var systemLabels = map[string]string{
	"INBOX": provider.LabelInbox,
	"SENT":  provider.LabelSent,
	"DRAFT": provider.LabelDrafts,
	"TRASH": provider.LabelTrash,
	"SPAM":  provider.LabelSpam,
}

var reservedToSystem = map[string]string{
	provider.LabelInbox:  "INBOX",
	provider.LabelSent:   "SENT",
	provider.LabelDrafts: "DRAFT",
	provider.LabelTrash:  "TRASH",
	provider.LabelSpam:   "SPAM",
}

func isReserved(l string) bool {
	for _, r := range provider.ReservedLabels {
		if strings.EqualFold(r, l) {
			return true
		}
	}
	return false
}

func hasLabel(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// convert maps a Gmail message. names resolves user label ids to names.
func (p *Provider) convert(g *gMessage, names map[string]string) provider.Message {
	pl := g.Payload
	m := provider.Message{
		ID:       g.ID,
		ThreadID: g.ThreadID,
		Provider: Name,
		Account:  p.alias,
		From:     parseAddr(pl.header("From")),
		To:       parseAddrList(pl.header("To")),
		Cc:       parseAddrList(pl.header("Cc")),
		Bcc:      parseAddrList(pl.header("Bcc")),
		ReplyTo:  parseAddrList(pl.header("Reply-To")),
		Subject:  decodeHeader(pl.header("Subject")),
		Snippet:  snippet(html.UnescapeString(g.Snippet)),
		WebLink:  messageLink(g.ID),
		Flags: provider.Flags{
			Unread:  hasLabel(g.LabelIDs, "UNREAD"),
			Draft:   hasLabel(g.LabelIDs, "DRAFT"),
			Flagged: hasLabel(g.LabelIDs, "STARRED"),
		},
	}
	if ms, err := strconv.ParseInt(g.InternalDate, 10, 64); err == nil {
		m.Date = time.UnixMilli(ms).UTC()
	} else if t, err := mail.ParseDate(pl.header("Date")); err == nil {
		m.Date = t.UTC()
	}
	located := false
	for _, id := range g.LabelIDs {
		if r, ok := systemLabels[id]; ok {
			m.Labels = append(m.Labels, r)
			located = true
		} else if n, ok := names[id]; ok {
			m.Labels = append(m.Labels, n)
		}
	}
	if !located && g.LabelIDs != nil {
		m.Labels = append([]string{provider.LabelArchive}, m.Labels...)
	}
	if pl != nil && (pl.Body.Data != "" || len(pl.Parts) > 0) {
		m.BodyText, m.BodyHTML = bodies(pl)
		m.Attachments = attachments(pl)
	}
	for _, a := range m.Attachments {
		if !a.Inline {
			m.Flags.HasAttachments = true
		}
	}
	if pl != nil && len(pl.Parts) == 0 && strings.EqualFold(pl.MimeType, "multipart/mixed") {
		m.Flags.HasAttachments = true // format=metadata: no parts, but mixed means files
	}
	if pl != nil {
		h := map[string]string{}
		for _, x := range pl.Headers {
			k := textproto.CanonicalMIMEHeaderKey(x.Name)
			if _, dup := h[k]; !dup && headerAllow[strings.ToLower(x.Name)] {
				h[k] = x.Value
			}
		}
		if len(h) > 0 {
			m.Headers = h
		}
	}
	return m
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
