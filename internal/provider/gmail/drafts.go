package gmail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/render"
)

// maxQuoted caps the original text quoted into a reply or forward.
const maxQuoted = 100000

// upload writes a draft through the media upload endpoint (multipart:
// JSON metadata + message/rfc822), which takes messages up to 35 MB.
// draftID "" creates a draft; otherwise the draft is replaced.
func (p *Provider) upload(ctx context.Context, draftID, threadID string, raw []byte) (*gDraft, error) {
	meta := map[string]any{"message": map[string]any{}}
	if threadID != "" {
		meta["message"] = map[string]any{"threadId": threadID}
	}
	method, path := http.MethodPost, "/upload"+userPath+"/drafts?uploadType=multipart"
	if draftID != "" {
		meta["id"] = draftID
		method, path = http.MethodPut, "/upload"+userPath+"/drafts/"+pathEsc(draftID)+"?uploadType=multipart"
	}
	mj, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_, _ = pw.Write(mj)
	pw, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"message/rfc822"}})
	_, _ = pw.Write(raw)
	_ = mw.Close()
	r, err := p.c.do(ctx, method, path, body.Bytes(), "multipart/related; boundary="+mw.Boundary())
	if err != nil {
		return nil, err
	}
	var d gDraft
	if err := decode(r, method, &d); err != nil {
		return nil, err
	}
	if d.ID == "" {
		return nil, errs.Provider("invalidResponse", fmt.Errorf("draft upload returned no id"))
	}
	return &d, nil
}

// draftResult re-reads the draft so the caller gets recipients, body and link.
func (p *Provider) draftResult(ctx context.Context, d *gDraft) (*provider.Message, error) {
	full, err := p.getDraft(ctx, d.ID, "full")
	if err != nil {
		return nil, fmt.Errorf("draft %s was saved but reading it back failed: %w", d.ID, err)
	}
	m := p.convertDraft(full, p.labelNames(ctx))
	m.Headers = nil
	return &m, nil
}

func (p *Provider) self() provider.Address { return provider.Address{Email: p.username} }

func (p *Provider) CreateDraft(ctx context.Context, in provider.DraftInput) (*provider.Message, error) {
	o := outgoing{From: p.self(), To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject, Body: in.Body, Attachments: in.Attachments}
	raw, err := o.build()
	if err != nil {
		return nil, err
	}
	d, err := p.upload(ctx, "", "", raw)
	if err != nil {
		return nil, err
	}
	return p.draftResult(ctx, d)
}

// original is what replies and forwards need from the message they answer.
type original struct {
	g         *gMessage
	msg       provider.Message
	messageID string
	refs      string
	text      string // visible text, hidden HTML text removed
}

func (p *Provider) loadOriginal(ctx context.Context, id string) (*original, error) {
	if isDraftID(id) {
		return nil, errs.Usage("%s is a draft; reply to or forward a received message", id)
	}
	g, err := p.getFull(ctx, id)
	if err != nil {
		return nil, err
	}
	o := &original{g: g, msg: p.convert(g, nil)}
	o.messageID = strings.TrimSpace(g.Payload.header("Message-Id"))
	o.refs = g.Payload.header("References")
	if strings.TrimSpace(o.msg.BodyHTML) != "" {
		o.text, _ = render.HTMLToText(o.msg.BodyHTML)
	} else {
		o.text = o.msg.BodyText
	}
	if r := []rune(o.text); len(r) > maxQuoted {
		o.text = string(r[:maxQuoted]) + "\n[…]"
	}
	return o, nil
}

func (o *original) dateString() string {
	if d := o.g.Payload.header("Date"); d != "" {
		return decodeHeader(d)
	}
	return o.msg.Date.Format("Mon, 2 Jan 2006 15:04:05 -0700")
}

// dedupe drops empty addresses, the mailbox itself, and repeats.
func dedupe(self string, lists ...[]provider.Address) []provider.Address {
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

// CreateReplyDraft builds the reply locally (Gmail has no server-side
// createReply): In-Reply-To/References from the original, the same
// threadId, and the original quoted below the agent's text.
func (p *Provider) CreateReplyDraft(ctx context.Context, in provider.ReplyInput) (*provider.Message, error) {
	o, err := p.loadOriginal(ctx, in.MessageID)
	if err != nil {
		return nil, err
	}
	to := o.msg.ReplyTo
	if len(to) == 0 {
		to = []provider.Address{o.msg.From}
	}
	if strings.EqualFold(o.msg.From.Email, p.username) {
		to = o.msg.To // replying to our own sent message: same recipients
	}
	to = dedupe(p.username, to)
	var cc []provider.Address
	if in.All {
		cc = dedupe(p.username, to, o.msg.To, o.msg.Cc)[len(to):]
	}
	if len(to) == 0 && len(cc) == 0 {
		return nil, errs.Usage("message %s has no one to reply to", in.MessageID)
	}
	body := in.Body
	if strings.TrimSpace(o.text) != "" {
		body = strings.TrimRight(body, "\n") + "\n\nOn " + o.dateString() + ", " + o.msg.From.String() + " wrote:\n" + quote(o.text) + "\n"
	}
	out := outgoing{From: p.self(), To: to, Cc: cc, Subject: replySubject(o.msg.Subject), Body: body,
		InReplyTo: o.messageID, References: references(o.refs, o.messageID), Attachments: in.Attachments}
	raw, err := out.build()
	if err != nil {
		return nil, err
	}
	d, err := p.upload(ctx, "", o.g.ThreadID, raw)
	if err != nil {
		return nil, err
	}
	return p.draftResult(ctx, d)
}

// CreateForwardDraft quotes the original as a forwarded block and carries
// its (non-inline) attachments, in the original thread.
func (p *Provider) CreateForwardDraft(ctx context.Context, in provider.ForwardInput) (*provider.Message, error) {
	o, err := p.loadOriginal(ctx, in.MessageID)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if strings.TrimSpace(in.Body) != "" {
		b.WriteString(strings.TrimRight(in.Body, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString("---------- Forwarded message ---------\n")
	fmt.Fprintf(&b, "From: %s\nDate: %s\nSubject: %s\n", o.msg.From.String(), o.dateString(), o.msg.Subject)
	if len(o.msg.To) > 0 {
		fmt.Fprintf(&b, "To: %s\n", formatAddrs(o.msg.To))
	}
	if len(o.msg.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\n", formatAddrs(o.msg.Cc))
	}
	b.WriteString("\n")
	b.WriteString(o.text)
	atts, err := p.fileParts(ctx, o.g)
	if err != nil {
		return nil, err
	}
	out := outgoing{From: p.self(), To: in.To, Subject: forwardSubject(o.msg.Subject), Body: b.String(),
		InReplyTo: o.messageID, References: references(o.refs, o.messageID), Attachments: atts}
	raw, err := out.build()
	if err != nil {
		return nil, err
	}
	d, err := p.upload(ctx, "", o.g.ThreadID, raw)
	if err != nil {
		return nil, err
	}
	return p.draftResult(ctx, d)
}

// fileParts downloads a message's non-inline attachments.
func (p *Provider) fileParts(ctx context.Context, g *gMessage) ([]provider.AttachmentFile, error) {
	var parts []*gPart
	walkParts(g.Payload, func(pt *gPart) {
		if isAttachment(pt) && !isInline(pt) {
			parts = append(parts, pt)
		}
	})
	var out []provider.AttachmentFile
	for _, pt := range parts {
		data, err := p.partData(ctx, g.ID, pt)
		if err != nil {
			return nil, err
		}
		out = append(out, provider.AttachmentFile{Name: pt.Filename, MIME: strings.ToLower(pt.MimeType), Data: data})
	}
	return out, nil
}

// resolveDraftID accepts a draft id or the message id of a draft (as
// `list --label drafts` returns) and returns the draft id.
func (p *Provider) resolveDraftID(ctx context.Context, id string) (string, error) {
	if isDraftID(id) {
		return id, nil
	}
	token := ""
	for range 20 {
		v := url.Values{}
		v.Set("maxResults", "500")
		if token != "" {
			v.Set("pageToken", token)
		}
		var res struct {
			Drafts        []gDraft `json:"drafts"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/drafts?"+v.Encode(), nil, &res); err != nil {
			return "", err
		}
		for _, d := range res.Drafts {
			if d.Message != nil && d.Message.ID == id {
				return d.ID, nil
			}
		}
		if token = res.NextPageToken; token == "" {
			break
		}
	}
	return "", errs.Usage("%s is not a draft", id)
}

// UpdateDraft rewrites the draft (Gmail replaces drafts whole). Fields not
// given are carried over from the current draft: recipients, subject,
// threading headers, the plain-text body and non-inline attachments. A
// draft written in Gmail with formatting keeps its text but loses the
// formatting and inline images.
func (p *Provider) UpdateDraft(ctx context.Context, id string, u provider.DraftUpdate) (*provider.Message, error) {
	draftID, err := p.resolveDraftID(ctx, id)
	if err != nil {
		return nil, err
	}
	d, err := p.getDraft(ctx, draftID, "full")
	if err != nil {
		return nil, err
	}
	cur := p.convert(d.Message, nil)
	pl := d.Message.Payload
	out := outgoing{From: p.self(), To: cur.To, Cc: cur.Cc, Bcc: cur.Bcc, Subject: cur.Subject, Body: cur.BodyText,
		InReplyTo: pl.header("In-Reply-To"), References: pl.header("References")}
	if out.Body == "" && strings.TrimSpace(cur.BodyHTML) != "" {
		out.Body, _ = render.HTMLToText(cur.BodyHTML)
	}
	if u.To != nil {
		out.To = *u.To
	}
	if u.Cc != nil {
		out.Cc = *u.Cc
	}
	if u.Bcc != nil {
		out.Bcc = *u.Bcc
	}
	if u.Subject != nil {
		out.Subject = *u.Subject
	}
	if u.Body != nil {
		out.Body = *u.Body
	}
	if out.Attachments, err = p.fileParts(ctx, d.Message); err != nil {
		return nil, err
	}
	out.Attachments = append(out.Attachments, u.Attachments...)
	raw, err := out.build()
	if err != nil {
		return nil, err
	}
	nd, err := p.upload(ctx, draftID, d.Message.ThreadID, raw)
	if err != nil {
		return nil, err
	}
	return p.draftResult(ctx, nd)
}

// ListDrafts lists drafts newest first; ids are draft ids.
func (p *Provider) ListDrafts(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	c := cursor{Max: limitOr(o.Limit, 20), Drafts: true}
	if o.Cursor != "" {
		var err error
		if c, err = decodeCursor(o.Cursor); err != nil {
			return nil, err
		}
		if !c.Drafts {
			return nil, errs.Usage("--cursor is not a draft list cursor")
		}
	}
	v := url.Values{}
	v.Set("maxResults", strconv.Itoa(c.Max))
	if c.Token != "" {
		v.Set("pageToken", c.Token)
	}
	var res struct {
		Drafts        []gDraft `json:"drafts"`
		NextPageToken string   `json:"nextPageToken"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/drafts?"+v.Encode(), nil, &res); err != nil {
		return nil, err
	}
	ds := make([]*gDraft, len(res.Drafts))
	errsOut := make([]error, len(res.Drafts))
	sem := make(chan struct{}, fetchConcurrency)
	var wg sync.WaitGroup
	for i := range res.Drafts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ds[i], errsOut[i] = p.getDraft(ctx, res.Drafts[i].ID, "metadata")
		}(i)
	}
	wg.Wait()
	names := p.labelNames(ctx)
	pg := &provider.Page{Messages: make([]provider.Message, 0, len(ds))}
	for i, d := range ds {
		if errsOut[i] != nil {
			if isNotFound(errsOut[i]) {
				continue
			}
			return nil, errsOut[i]
		}
		m := p.convertDraft(d, names)
		m.Headers = nil
		pg.Messages = append(pg.Messages, m)
	}
	if res.NextPageToken != "" {
		c.Token = res.NextPageToken
		pg.NextCursor = c.encode()
	}
	return pg, nil
}

func (p *Provider) DeleteDraft(ctx context.Context, id string) error {
	draftID, err := p.resolveDraftID(ctx, id)
	if err != nil {
		return err
	}
	_, err = p.c.do(ctx, http.MethodDelete, userPath+"/drafts/"+pathEsc(draftID), nil, "")
	return err
}

// Send sends an existing draft with drafts.send; Gmail deletes the draft
// and returns the sent message.
func (p *Provider) Send(ctx context.Context, id string) (*provider.SendResult, error) {
	draftID, err := p.resolveDraftID(ctx, id)
	if err != nil {
		return nil, err
	}
	var g gMessage
	if err := p.c.doJSON(ctx, http.MethodPost, userPath+"/drafts/send", map[string]string{"id": draftID}, &g); err != nil {
		return nil, err
	}
	res := &provider.SendResult{MessageID: g.ID, ThreadID: g.ThreadID, SentAt: time.Now().UTC().Truncate(time.Second)}
	// Best effort: the Message-ID Gmail assigned. The send already happened,
	// so a failure here must not turn into an error.
	var meta gMessage
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/messages/"+pathEsc(g.ID)+"?format=metadata&metadataHeaders=Message-ID", nil, &meta); err == nil {
		res.InternetMessageID = meta.Payload.header("Message-Id")
	}
	return res, nil
}
