package mailmime

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strconv"
	"strings"

	"golang.org/x/net/html/charset"

	"github.com/james-tronai/email-cli/internal/provider"
)

// maxDepth bounds multipart nesting so a hostile message cannot recurse
// without end.
const maxDepth = 20

// Part is one MIME part of a parsed message. Path is the IMAP section
// number ("1", "2.1"); a multipart container has no data of its own.
type Part struct {
	Path      string
	Header    textproto.MIMEHeader
	MediaType string // lower case, e.g. "text/plain"
	Params    map[string]string
	Filename  string
	Data      []byte // after transfer decoding (leaves only)
	Parts     []*Part
}

// Get returns the first value of a header.
func (p *Part) Get(name string) string { return p.Header.Get(name) }

// Message is a parsed RFC 822 message.
type Message struct {
	Header textproto.MIMEHeader
	Root   *Part
}

// ParseHeader parses only a header block (as from BODY[HEADER]).
func ParseHeader(raw []byte) textproto.MIMEHeader {
	r := textproto.NewReader(bufio.NewReader(io.MultiReader(bytes.NewReader(raw), strings.NewReader("\r\n\r\n"))))
	h, err := r.ReadMIMEHeader()
	if h == nil && err != nil {
		return textproto.MIMEHeader{}
	}
	return h
}

// Parse splits a message into parts. Damaged structure does not fail the
// parse: whatever cannot be split stays one opaque part.
func Parse(raw []byte) *Message {
	header, body := splitHeader(raw)
	root := newPart(header)
	m := &Message{Header: header, Root: root}
	if strings.HasPrefix(root.MediaType, "multipart/") {
		parseMultipart(root, body, "", 0)
	} else {
		root.Path = "1"
		root.Data = decodeTransfer(header.Get("Content-Transfer-Encoding"), body)
	}
	return m
}

func splitHeader(raw []byte) (textproto.MIMEHeader, []byte) {
	br := bufio.NewReader(bytes.NewReader(raw))
	h, err := textproto.NewReader(br).ReadMIMEHeader()
	if h == nil {
		h = textproto.MIMEHeader{}
	}
	if err != nil && err != io.EOF {
		return h, nil
	}
	rest, _ := io.ReadAll(br)
	return h, rest
}

func newPart(h textproto.MIMEHeader) *Part {
	p := &Part{Header: h}
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt, params = "application/octet-stream", map[string]string{}
		if i := strings.IndexByte(ct, ';'); i > 0 {
			mt = strings.ToLower(strings.TrimSpace(ct[:i]))
		}
	}
	p.MediaType, p.Params = strings.ToLower(mt), params
	if _, dp, err := mime.ParseMediaType(h.Get("Content-Disposition")); err == nil && dp["filename"] != "" {
		p.Filename = DecodeHeader(dp["filename"])
	} else if params["name"] != "" {
		p.Filename = DecodeHeader(params["name"])
	}
	return p
}

func childPath(parent string, i int) string {
	if parent == "" {
		return strconv.Itoa(i)
	}
	return parent + "." + strconv.Itoa(i)
}

func parseMultipart(p *Part, body []byte, path string, depth int) {
	p.Path = path
	boundary := p.Params["boundary"]
	if boundary == "" || depth >= maxDepth {
		p.Data = body
		return
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for i := 1; ; i++ {
		raw, err := mr.NextRawPart()
		if err != nil {
			return // io.EOF, or a damaged part: keep what was read
		}
		data, err := io.ReadAll(raw)
		if err != nil {
			return
		}
		child := newPart(raw.Header)
		cp := childPath(path, i)
		if strings.HasPrefix(child.MediaType, "multipart/") {
			parseMultipart(child, data, cp, depth+1)
		} else {
			child.Path = cp
			child.Data = decodeTransfer(raw.Header.Get("Content-Transfer-Encoding"), data)
		}
		p.Parts = append(p.Parts, child)
	}
}

func decodeTransfer(enc string, data []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, data)
		out := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		n, err := base64.StdEncoding.Decode(out, clean)
		if err != nil {
			n, _ = base64.RawStdEncoding.Decode(out, bytes.TrimRight(clean, "="))
		}
		return out[:n]
	case "quoted-printable":
		b, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
		if err != nil && len(b) == 0 {
			return data
		}
		return b
	}
	return data
}

// Walk visits every leaf part.
func (p *Part) Walk(fn func(*Part)) {
	if p == nil {
		return
	}
	if len(p.Parts) == 0 {
		fn(p)
		return
	}
	for _, c := range p.Parts {
		c.Walk(fn)
	}
}

// Text decodes a text part to UTF-8.
func (p *Part) Text() string {
	cs := strings.ToLower(p.Params["charset"])
	if cs == "" || cs == "utf-8" || cs == "us-ascii" || cs == "utf8" {
		return string(p.Data)
	}
	r, err := charset.NewReaderLabel(cs, bytes.NewReader(p.Data))
	if err != nil {
		return string(p.Data)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return string(p.Data)
	}
	return string(b)
}

// IsAttachment reports whether a part is a file rather than body text:
// it has a name or an attachment disposition, or it is not text (an
// inline image without a file name is still a file).
func (p *Part) IsAttachment() bool {
	if p.Filename != "" || !strings.HasPrefix(p.MediaType, "text/") {
		return true
	}
	disp, _, _ := mime.ParseMediaType(p.Get("Content-Disposition"))
	return disp == "attachment"
}

func (p *Part) IsInline() bool {
	disp, _, _ := mime.ParseMediaType(p.Get("Content-Disposition"))
	if disp == "inline" {
		return true
	}
	return disp == "" && p.Get("Content-Id") != ""
}

// Name is the attachment's file name, with a fallback for unnamed parts.
func (p *Part) Name() string {
	if p.Filename != "" {
		return p.Filename
	}
	if p.MediaType == "message/rfc822" {
		return "message.eml"
	}
	return "part-" + p.Path
}

// Bodies returns the first text/plain and text/html body parts.
func (m *Message) Bodies() (text, html string) {
	var haveText, haveHTML bool
	m.Root.Walk(func(p *Part) {
		if p.IsAttachment() {
			return
		}
		switch p.MediaType {
		case "text/plain":
			if !haveText {
				text, haveText = p.Text(), true
			}
		case "text/html":
			if !haveHTML {
				html, haveHTML = p.Text(), true
			}
		}
	})
	return text, html
}

// Attachments lists file parts; the id is the part path.
func (m *Message) Attachments() []provider.AttachmentMeta {
	var out []provider.AttachmentMeta
	m.Root.Walk(func(p *Part) {
		if !p.IsAttachment() {
			return
		}
		out = append(out, provider.AttachmentMeta{ID: p.Path, Name: p.Name(), Size: int64(len(p.Data)),
			MIME: p.MediaType, Inline: p.IsInline()})
	})
	return out
}

// Find returns the leaf part with the given path.
func (m *Message) Find(path string) *Part {
	var hit *Part
	m.Root.Walk(func(p *Part) {
		if hit == nil && p.Path == path {
			hit = p
		}
	})
	return hit
}

// Files returns the non-inline attachments, for carrying them into a
// forward or a rewritten draft.
func (m *Message) Files() []provider.AttachmentFile {
	var out []provider.AttachmentFile
	m.Root.Walk(func(p *Part) {
		if p.IsAttachment() && !p.IsInline() {
			out = append(out, provider.AttachmentFile{Name: p.Name(), MIME: p.MediaType, Data: p.Data})
		}
	})
	return out
}
