package imap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/mailmime"
	"github.com/james-tronai/email-cli/internal/provider"
)

// cursor pages through one search, newest first. It is self-contained:
// the next page re-runs the search below the last UID returned.
type cursor struct {
	Box      string `json:"b"`
	Validity uint32 `json:"v"`
	Raw      string `json:"q,omitempty"` // Gmail X-GM-RAW
	Terms    []term `json:"c,omitempty"`
	Unseen   bool   `json:"u,omitempty"`
	Since    int64  `json:"s,omitempty"`
	Before   uint32 `json:"t,omitempty"` // UIDs below this one
	Max      int    `json:"n"`
	Drafts   bool   `json:"d,omitempty"`
}

// term is one generic SEARCH key: FROM, TO, CC, SUBJECT, BODY, TEXT, SINCE,
// BEFORE (dates as 2006-01-02), UNSEEN, SEEN, FLAGGED.
type term struct {
	K string `json:"k"`
	V string `json:"v,omitempty"`
}

const cursorPrefix = "im1."

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, cursorPrefix))
	if !strings.HasPrefix(s, cursorPrefix) || err != nil || json.Unmarshal(b, &c) != nil || c.Box == "" {
		return c, errs.Usage("invalid --cursor; pass next_cursor exactly as returned")
	}
	return c, nil
}

func limitOr(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 500)
}

// archiveQuery selects mail that is in no system folder: Gmail's "archive".
const archiveQuery = "-in:inbox -in:sent -in:drafts -in:spam -in:trash -in:chats"

// List lists a mailbox (or Gmail label) newest first.
func (p *Provider) List(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	if o.Cursor != "" {
		c, err := decodeCursor(o.Cursor)
		if err != nil {
			return nil, err
		}
		if c.Drafts {
			return nil, errs.Usage("--cursor is from draft list; use it with `draft list`")
		}
		return p.page(ctx, c)
	}
	if _, err := p.conn(ctx); err != nil {
		return nil, err
	}
	label := o.Label
	if label == "" {
		label = provider.LabelInbox
	}
	c := cursor{Max: limitOr(o.Limit, 20), Unseen: o.Unread}
	if !o.Since.IsZero() {
		c.Since = o.Since.Unix()
	}
	var err error
	if p.gmail && strings.EqualFold(label, provider.LabelArchive) {
		c.Box, err = p.mustRole(ctx, "all")
		c.Raw = archiveQuery
	} else {
		c.Box, err = p.labelBox(ctx, label)
	}
	if err != nil {
		return nil, err
	}
	return p.page(ctx, c)
}

// Search: on Gmail the query is Gmail search box syntax (X-GM-RAW) over
// All Mail. Elsewhere a small subset is understood — from: to: cc:
// subject: body: in:<label> is:unread is:flagged since:/before:YYYY-MM-DD —
// and the remaining words are matched with TEXT, in INBOX unless in: says
// otherwise.
func (p *Provider) Search(ctx context.Context, o provider.SearchOptions) (*provider.Page, error) {
	if o.Cursor != "" {
		c, err := decodeCursor(o.Cursor)
		if err != nil {
			return nil, err
		}
		return p.page(ctx, c)
	}
	if strings.TrimSpace(o.Query) == "" {
		return nil, errs.Usage("search query is empty")
	}
	if _, err := p.conn(ctx); err != nil {
		return nil, err
	}
	c := cursor{Max: limitOr(o.Limit, 20)}
	var err error
	if p.gmail {
		c.Box, err = p.mustRole(ctx, "all")
		c.Raw = o.Query
	} else {
		var label string
		c.Terms, label, err = parseQuery(o.Query)
		if err == nil {
			c.Box, err = p.labelBox(ctx, label)
		}
	}
	if err != nil {
		return nil, err
	}
	return p.page(ctx, c)
}

// parseQuery turns the generic query subset into SEARCH terms.
func parseQuery(qs string) ([]term, string, error) {
	label := provider.LabelInbox
	var terms []term
	var text []string
	for _, tok := range splitQuery(qs) {
		k, v, ok := strings.Cut(tok, ":")
		if !ok || v == "" {
			text = append(text, tok)
			continue
		}
		switch strings.ToLower(k) {
		case "from", "to", "cc", "subject", "body":
			terms = append(terms, term{K: strings.ToUpper(k), V: v})
		case "in", "label":
			label = v
		case "is":
			switch strings.ToLower(v) {
			case "unread":
				terms = append(terms, term{K: "UNSEEN"})
			case "read":
				terms = append(terms, term{K: "SEEN"})
			case "flagged", "starred":
				terms = append(terms, term{K: "FLAGGED"})
			default:
				return nil, "", errs.Usage("unsupported is:%s (want unread, read or flagged)", v)
			}
		case "since", "after", "before":
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return nil, "", errs.Usage("%s: wants a date like 2026-10-01", k)
			}
			key := "SINCE"
			if strings.EqualFold(k, "before") {
				key = "BEFORE"
			}
			terms = append(terms, term{K: key, V: v})
		default:
			text = append(text, tok)
		}
	}
	for _, t := range text {
		terms = append(terms, term{K: "TEXT", V: t})
	}
	return terms, label, nil
}

// splitQuery splits on spaces, keeping "quoted phrases" (also after key:).
func splitQuery(s string) []string {
	var out []string
	var b strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
		case r == ' ' && !inQ:
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func imapDate(s string) string {
	t, _ := time.Parse("2006-01-02", s)
	return t.Format("2-Jan-2006")
}

// criteria builds the SEARCH arguments. A cursor comes back from the
// caller, so its search keys are checked: they are written unquoted.
func (c cursor) criteria(gmail bool) ([]any, error) {
	var out []any
	rawQ := c.Raw
	if c.Since != 0 {
		if gmail {
			rawQ = strings.TrimSpace(rawQ + " after:" + strconv.FormatInt(c.Since-1, 10))
		} else {
			out = append(out, raw("SINCE"), raw(time.Unix(c.Since, 0).UTC().Format("2-Jan-2006")))
		}
	}
	if rawQ != "" {
		out = append(out, raw("X-GM-RAW"), q(rawQ))
	}
	if c.Unseen {
		out = append(out, raw("UNSEEN"))
	}
	for _, t := range c.Terms {
		switch t.K {
		case "UNSEEN", "SEEN", "FLAGGED":
			out = append(out, raw(t.K))
		case "SINCE", "BEFORE":
			if _, err := time.Parse("2006-01-02", t.V); err != nil {
				return nil, errs.Usage("invalid --cursor")
			}
			out = append(out, raw(t.K), raw(imapDate(t.V)))
		case "FROM", "TO", "CC", "SUBJECT", "BODY", "TEXT":
			out = append(out, raw(t.K), q(t.V))
		default:
			return nil, errs.Usage("invalid --cursor")
		}
	}
	if c.Before > 0 {
		if c.Before <= 1 {
			return nil, nil
		}
		out = append(out, raw("UID"), raw("1:"+strconv.FormatUint(uint64(c.Before-1), 10)))
	}
	if len(out) == 0 {
		out = append(out, raw("ALL"))
	}
	return out, nil
}

func (p *Provider) page(ctx context.Context, c cursor) (*provider.Page, error) {
	pg := &provider.Page{Messages: []provider.Message{}}
	if _, err := p.conn(ctx); err != nil {
		return nil, err
	}
	crit, err := c.criteria(p.gmail)
	if err != nil {
		return nil, err
	}
	if crit == nil {
		return pg, nil
	}
	uids, err := p.search(ctx, c.Box, crit...)
	if err != nil {
		return nil, err
	}
	validity := p.c.uidValidity
	if c.Validity != 0 && c.Validity != validity {
		return nil, errs.Usage("mailbox %q changed on the server since this cursor was made; list again without --cursor", c.Box)
	}
	c.Validity = validity
	slices.Reverse(uids)
	more := len(uids) > c.Max
	if more {
		uids = uids[:c.Max]
	}
	fs, err := p.fetch(ctx, c.Box, uids, false)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if c.Since != 0 && !p.gmail && !f.date.IsZero() && f.date.Unix() < c.Since {
			continue // SINCE matches whole days
		}
		m := p.convert(ctx, f)
		m.Headers = nil
		pg.Messages = append(pg.Messages, m)
	}
	if more {
		c.Before = uids[len(uids)-1]
		pg.NextCursor = c.encode()
	}
	return pg, nil
}

// Get fetches one message (or draft) with its body.
func (p *Provider) Get(ctx context.Context, id string, o provider.GetOptions) (*provider.Message, error) {
	f, err := p.fetchOne(ctx, id, true)
	if err != nil {
		return nil, err
	}
	m := p.convert(ctx, f)
	if !o.Headers {
		m.Headers = nil
	}
	return &m, nil
}

const maxThreadMessages = 200

// Thread returns a conversation oldest first. On Gmail it is X-GM-THRID
// in All Mail and Drafts; elsewhere every message in INBOX, Sent, Drafts
// and Archive whose Message-ID, References or In-Reply-To names the root.
func (p *Provider) Thread(ctx context.Context, threadID string, limit int) ([]provider.Message, error) {
	if _, err := p.conn(ctx); err != nil {
		return nil, err
	}
	var boxes []string
	var crit []any
	if p.gmail {
		n, err := parseHexID(threadID)
		if err != nil {
			return nil, err
		}
		crit = []any{raw("X-GM-THRID"), raw(strconv.FormatUint(n, 10))}
		for _, role := range []string{"all", provider.LabelDrafts} {
			if b, err := p.byRole(ctx, role); err != nil {
				return nil, err
			} else if b != "" {
				boxes = append(boxes, b)
			}
		}
	} else {
		root := strings.TrimSpace(threadID)
		if !strings.HasPrefix(root, "<") || !strings.HasSuffix(root, ">") {
			return nil, errs.Usage("%q is not a thread id of this account (a <Message-ID>)", threadID)
		}
		crit = []any{raw("OR OR HEADER Message-ID"), q(root), raw("HEADER References"), q(root), raw("HEADER In-Reply-To"), q(root)}
		boxes = []string{"INBOX"}
		for _, role := range []string{provider.LabelSent, provider.LabelDrafts, provider.LabelArchive} {
			if b, err := p.byRole(ctx, role); err != nil {
				return nil, err
			} else if b != "" && !slices.Contains(boxes, b) {
				boxes = append(boxes, b)
			}
		}
	}
	seen := map[string]bool{}
	var out []provider.Message
	for _, box := range boxes {
		uids, err := p.search(ctx, box, crit...)
		if err != nil {
			return nil, err
		}
		if len(uids) > maxThreadMessages {
			uids = uids[len(uids)-maxThreadMessages:]
		}
		fs, err := p.fetch(ctx, box, uids, true)
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			key := hexID(f.msgID)
			if !p.gmail {
				key = strings.TrimSpace(f.header.Get("Message-Id"))
				if key == "" {
					key = p.id(f)
				}
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			m := p.convert(ctx, f)
			m.Headers = nil
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, errs.NotFound("notFound", fmt.Errorf("thread %s not found", threadID))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	if len(out) > maxThreadMessages {
		out = out[len(out)-maxThreadMessages:]
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// ---- attachments ----

func (p *Provider) parsed(ctx context.Context, id string) (*mailmime.Message, error) {
	f, err := p.fetchOne(ctx, id, true)
	if err != nil {
		return nil, err
	}
	return mailmime.Parse(f.raw), nil
}

func (p *Provider) ListAttachments(ctx context.Context, id string) ([]provider.AttachmentMeta, error) {
	pm, err := p.parsed(ctx, id)
	if err != nil {
		return nil, err
	}
	out := pm.Attachments()
	if out == nil {
		out = []provider.AttachmentMeta{}
	}
	return out, nil
}

// GetAttachment takes the part path ("2", "1.3") that ListAttachments
// returns as the id.
func (p *Provider) GetAttachment(ctx context.Context, id, partID string) (*provider.Attachment, error) {
	pm, err := p.parsed(ctx, id)
	if err != nil {
		return nil, err
	}
	pt := pm.Find(partID)
	if pt == nil || !pt.IsAttachment() {
		return nil, errs.NotFound("notFound", fmt.Errorf("attachment %s not found on %s", partID, id))
	}
	return &provider.Attachment{
		AttachmentMeta: provider.AttachmentMeta{ID: pt.Path, Name: pt.Name(), Size: int64(len(pt.Data)), MIME: pt.MediaType, Inline: pt.IsInline()},
		Data:           pt.Data,
	}, nil
}
