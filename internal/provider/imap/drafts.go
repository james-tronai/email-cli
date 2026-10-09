package imap

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/mailmime"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/render"
)

// maxQuoted caps the original text quoted into a reply or forward.
const maxQuoted = 100000

func (p *Provider) self() provider.Address { return provider.Address{Email: p.cfg.Username} }

func (p *Provider) domain() string {
	if i := strings.LastIndexByte(p.cfg.Username, '@'); i >= 0 {
		return strings.ToLower(p.cfg.Username[i+1:])
	}
	return ""
}

func (p *Provider) requireDraftMode() error {
	if p.cfg.Mode < policy.ModeDraft {
		return errs.Policy("account is in %s mode; the IMAP provider does not change mail below draft mode", p.cfg.Mode)
	}
	return nil
}

// appendDraft stores a message in Drafts and returns the stored copy.
func (p *Provider) appendDraft(ctx context.Context, msg []byte, messageID string) (*fetched, error) {
	if err := p.requireDraftMode(); err != nil {
		return nil, err
	}
	box, err := p.mustRole(ctx, provider.LabelDrafts)
	if err != nil {
		return nil, err
	}
	uid, err := p.appendTo(ctx, box, `(\Draft \Seen)`, msg, messageID)
	if err != nil {
		return nil, err
	}
	fs, err := p.fetch(ctx, box, []uint32{uid}, true)
	if err != nil {
		return nil, fmt.Errorf("the draft was saved but reading it back failed: %w", err)
	}
	if len(fs) == 0 {
		return nil, errs.Provider("notFound", errors.New("the draft was saved but is not in the Drafts mailbox"))
	}
	return fs[0], nil
}

// appendTo APPENDs a message and returns its UID: from APPENDUID
// (UIDPLUS), else by searching for its Message-ID.
func (p *Provider) appendTo(ctx context.Context, box, flags string, msg []byte, messageID string) (uint32, error) {
	c, err := p.conn(ctx)
	if err != nil {
		return 0, err
	}
	if c.selected == box {
		c.selected = "" // re-select so EXISTS / UIDNEXT are fresh
	}
	rs, err := c.run(ctx, raw("APPEND"), q(encodeMUTF7(box)), raw(flags), lit(msg))
	if err != nil {
		return 0, err
	}
	for _, r := range rs {
		if r.Code == "APPENDUID" && len(r.CodeArgs) >= 2 {
			if uid := asUint(atom(r.CodeArgs[1])); uid > 0 {
				return uint32(uid), nil
			}
		}
	}
	if messageID == "" {
		return 0, errs.Provider("noAppendUID", errors.New("the server did not report the new message's UID"))
	}
	uids, err := p.search(ctx, box, raw("HEADER Message-ID"), q(messageID))
	if err != nil {
		return 0, err
	}
	if len(uids) == 0 {
		return 0, errs.Provider("notFound", fmt.Errorf("the appended message %s is not in %q", messageID, box))
	}
	return uids[len(uids)-1], nil
}

func (p *Provider) draftResult(ctx context.Context, f *fetched) *provider.Message {
	m := p.convert(ctx, f)
	m.Flags.Draft = true
	m.Headers = nil
	return &m
}

func (p *Provider) buildAndAppend(ctx context.Context, o *mailmime.Outgoing) (*provider.Message, error) {
	o.From = p.self()
	o.MessageID = mailmime.NewMessageID(p.domain())
	o.Date = time.Now()
	msg, err := o.Build(MaxMessageSize, "the IMAP provider")
	if err != nil {
		return nil, err
	}
	f, err := p.appendDraft(ctx, msg, o.MessageID)
	if err != nil {
		return nil, err
	}
	return p.draftResult(ctx, f), nil
}

func (p *Provider) CreateDraft(ctx context.Context, in provider.DraftInput) (*provider.Message, error) {
	return p.buildAndAppend(ctx, &mailmime.Outgoing{To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject,
		Body: in.Body, Attachments: in.Attachments})
}

// original is what replies and forwards need from the message they answer.
type original struct {
	msg       provider.Message
	parsed    *mailmime.Message
	messageID string
	refs      string
	date      string
	text      string // visible text, hidden HTML text removed
}

func (p *Provider) loadOriginal(ctx context.Context, id string) (*original, error) {
	f, err := p.fetchOne(ctx, id, true)
	if err != nil {
		return nil, err
	}
	o := &original{msg: p.convert(ctx, f), parsed: mailmime.Parse(f.raw)}
	if o.msg.Flags.Draft {
		return nil, errs.Usage("%s is a draft; reply to or forward a received message", id)
	}
	o.messageID = strings.TrimSpace(f.header.Get("Message-Id"))
	o.refs = f.header.Get("References")
	o.date = mailmime.DecodeHeader(f.header.Get("Date"))
	if o.date == "" {
		o.date = o.msg.Date.Format("Mon, 2 Jan 2006 15:04:05 -0700")
	}
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

// CreateReplyDraft writes the reply with In-Reply-To / References, so the
// server (Gmail) threads it, and quotes the original's visible text.
func (p *Provider) CreateReplyDraft(ctx context.Context, in provider.ReplyInput) (*provider.Message, error) {
	if err := p.requireDraftMode(); err != nil {
		return nil, err
	}
	o, err := p.loadOriginal(ctx, in.MessageID)
	if err != nil {
		return nil, err
	}
	self := p.cfg.Username
	to := o.msg.ReplyTo
	if len(to) == 0 {
		to = []provider.Address{o.msg.From}
	}
	if strings.EqualFold(o.msg.From.Email, self) {
		to = o.msg.To // replying to our own sent message: same recipients
	}
	to = mailmime.Dedupe(self, to)
	var cc []provider.Address
	if in.All {
		cc = mailmime.Dedupe(self, to, o.msg.To, o.msg.Cc)[len(to):]
	}
	if len(to) == 0 && len(cc) == 0 {
		return nil, errs.Usage("message %s has no one to reply to", in.MessageID)
	}
	body := in.Body
	if strings.TrimSpace(o.text) != "" {
		body = strings.TrimRight(body, "\n") + "\n\nOn " + o.date + ", " + o.msg.From.String() + " wrote:\n" + mailmime.Quote(o.text) + "\n"
	}
	return p.buildAndAppend(ctx, &mailmime.Outgoing{To: to, Cc: cc, Subject: mailmime.ReplySubject(o.msg.Subject), Body: body,
		InReplyTo: o.messageID, References: mailmime.References(o.refs, o.messageID), Attachments: in.Attachments})
}

// CreateForwardDraft quotes the original as a forwarded block and carries
// its (non-inline) attachments.
func (p *Provider) CreateForwardDraft(ctx context.Context, in provider.ForwardInput) (*provider.Message, error) {
	if err := p.requireDraftMode(); err != nil {
		return nil, err
	}
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
	fmt.Fprintf(&b, "From: %s\nDate: %s\nSubject: %s\n", o.msg.From.String(), o.date, o.msg.Subject)
	if len(o.msg.To) > 0 {
		fmt.Fprintf(&b, "To: %s\n", mailmime.FormatAddrs(o.msg.To))
	}
	if len(o.msg.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\n", mailmime.FormatAddrs(o.msg.Cc))
	}
	b.WriteString("\n")
	b.WriteString(o.text)
	return p.buildAndAppend(ctx, &mailmime.Outgoing{To: in.To, Subject: mailmime.ForwardSubject(o.msg.Subject), Body: b.String(),
		InReplyTo: o.messageID, References: mailmime.References(o.refs, o.messageID), Attachments: o.parsed.Files()})
}

// draftAt locates a draft in the Drafts mailbox. Gmail also shows drafts
// in All Mail, so a Gmail id is looked up in Drafts directly.
func (p *Provider) draftAt(ctx context.Context, id string) (location, error) {
	box, err := p.mustRole(ctx, provider.LabelDrafts)
	if err != nil {
		return location{}, err
	}
	l, ok, err := p.inDrafts(ctx, id, box)
	if err != nil {
		return l, err
	}
	if !ok {
		if _, err := p.locate(ctx, id); err != nil {
			return location{}, err
		}
		return location{}, errs.Usage("%s is not a draft", id)
	}
	return l, nil
}

func (p *Provider) inDrafts(ctx context.Context, id, drafts string) (location, bool, error) {
	if !p.gmail {
		l, err := p.locate(ctx, id)
		return l, err == nil && l.box == drafts, err
	}
	n, err := parseHexID(id)
	if err != nil {
		return location{}, false, err
	}
	uids, err := p.search(ctx, drafts, raw("X-GM-MSGID"), raw(strconv.FormatUint(n, 10)))
	if err != nil || len(uids) == 0 {
		return location{}, false, err
	}
	return location{box: drafts, uid: uids[0]}, true, nil
}

// UpdateDraft writes a new version of the draft and removes the old one
// (IMAP messages cannot be changed in place), so the draft gets a new id.
// Fields not given are carried over: recipients, subject, threading
// headers, the plain-text body and non-inline attachments.
func (p *Provider) UpdateDraft(ctx context.Context, id string, u provider.DraftUpdate) (*provider.Message, error) {
	if err := p.requireDraftMode(); err != nil {
		return nil, err
	}
	l, err := p.draftAt(ctx, id)
	if err != nil {
		return nil, err
	}
	fs, err := p.fetch(ctx, l.box, []uint32{l.uid}, true)
	if err != nil {
		return nil, err
	}
	if len(fs) == 0 {
		return nil, errs.Provider("notFound", fmt.Errorf("draft %s not found", id))
	}
	cur := p.convert(ctx, fs[0])
	pm := mailmime.Parse(fs[0].raw)
	out := mailmime.Outgoing{To: cur.To, Cc: cur.Cc, Bcc: cur.Bcc, Subject: cur.Subject, Body: cur.BodyText,
		InReplyTo: fs[0].header.Get("In-Reply-To"), References: fs[0].header.Get("References")}
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
	out.Attachments = append(pm.Files(), u.Attachments...)
	m, err := p.buildAndAppend(ctx, &out)
	if err != nil {
		return nil, err
	}
	if err := p.remove(ctx, id, l); err != nil {
		return nil, fmt.Errorf("the new version %s was saved but the old draft %s could not be removed: %w", m.ID, id, err)
	}
	return m, nil
}

func (p *Provider) ListDrafts(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	if o.Cursor != "" {
		c, err := decodeCursor(o.Cursor)
		if err != nil {
			return nil, err
		}
		if !c.Drafts {
			return nil, errs.Usage("--cursor is not a draft list cursor")
		}
		return p.page(ctx, c)
	}
	box, err := p.mustRole(ctx, provider.LabelDrafts)
	if err != nil {
		return nil, err
	}
	return p.page(ctx, cursor{Box: box, Max: limitOr(o.Limit, 20), Drafts: true})
}

func (p *Provider) DeleteDraft(ctx context.Context, id string) error {
	if err := p.requireDraftMode(); err != nil {
		return err
	}
	l, err := p.draftAt(ctx, id)
	if err != nil {
		return err
	}
	return p.remove(ctx, id, l)
}

// remove deletes one message for good. On Gmail, expunging from a label
// folder only removes the label, so the message is moved to Trash and
// expunged there (Gmail's documented way to delete). Elsewhere it is
// flagged \Deleted and expunged by UID.
func (p *Provider) remove(ctx context.Context, id string, l location) error {
	p.forget(id)
	if p.gmail {
		trash, err := p.mustRole(ctx, provider.LabelTrash)
		if err != nil {
			return err
		}
		msgID, err := parseHexID(id)
		if err != nil {
			return err
		}
		if l.box != trash {
			if err := p.moveUID(ctx, l.box, l.uid, trash); err != nil {
				return err
			}
		}
		uids, err := p.search(ctx, trash, raw("X-GM-MSGID"), raw(strconv.FormatUint(msgID, 10)))
		if err != nil {
			return err
		}
		if len(uids) == 0 {
			return nil // already gone
		}
		return p.expungeUIDs(ctx, trash, uids)
	}
	return p.expungeUIDs(ctx, l.box, []uint32{l.uid})
}

// expungeUIDs removes exactly these messages. Without UIDPLUS a plain
// EXPUNGE would also remove anything else flagged \Deleted, so it is only
// used when nothing else is.
func (p *Provider) expungeUIDs(ctx context.Context, box string, uids []uint32) error {
	c, err := p.selectBox(ctx, box, true)
	if err != nil {
		return err
	}
	set := raw(uidSet(uids))
	if _, err := c.run(ctx, raw("UID STORE"), set, raw(`+FLAGS.SILENT (\Deleted)`)); err != nil {
		return err
	}
	if c.has("UIDPLUS") {
		_, err = c.run(ctx, raw("UID EXPUNGE"), set)
		return err
	}
	others, err := p.searchSelected(ctx, c, raw("DELETED NOT UID"), set)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		return errs.Provider("noUIDPlus", fmt.Errorf("mailbox %q holds other messages marked deleted and the server lacks UIDPLUS; not expunging", box))
	}
	_, err = c.run(ctx, raw("EXPUNGE"))
	return err
}

// searchSelected searches the already selected mailbox (keeping a
// read-write selection).
func (p *Provider) searchSelected(ctx context.Context, c *conn, criteria ...any) ([]uint32, error) {
	rs, err := c.run(ctx, append([]any{raw("UID SEARCH")}, criteria...)...)
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, r := range rs {
		if r.Kind == "SEARCH" {
			for _, f := range r.Fields {
				if n := asUint(f); n > 0 {
					out = append(out, uint32(n))
				}
			}
		}
	}
	return out, nil
}

// moveUID moves one message; it returns the UID in the destination when
// the server reports it (COPYUID), else 0.
func (p *Provider) moveUID(ctx context.Context, box string, uid uint32, dest string) error {
	_, err := p.moveUIDGet(ctx, box, uid, dest)
	return err
}

func (p *Provider) moveUIDGet(ctx context.Context, box string, uid uint32, dest string) (uint32, error) {
	c, err := p.selectBox(ctx, box, true)
	if err != nil {
		return 0, err
	}
	set := raw(strconv.FormatUint(uint64(uid), 10))
	var rs []*response
	if c.has("MOVE") {
		rs, err = c.run(ctx, raw("UID MOVE"), set, q(encodeMUTF7(dest)))
		if err != nil {
			return 0, err
		}
	} else {
		if rs, err = c.run(ctx, raw("UID COPY"), set, q(encodeMUTF7(dest))); err != nil {
			return 0, err
		}
		if err := p.expungeUIDs(ctx, box, []uint32{uid}); err != nil {
			return 0, err
		}
	}
	for _, r := range rs {
		if r.Code == "COPYUID" && len(r.CodeArgs) >= 3 {
			if n := asUint(atom(r.CodeArgs[2])); n > 0 {
				return uint32(n), nil
			}
		}
	}
	return 0, nil
}
