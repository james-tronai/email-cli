// Package imap is the provider for mailboxes reached over IMAP and SMTP
// with a password: personal Gmail with an app password, or any IMAP
// server. When the server announces X-GM-EXT-1 (Gmail), ids, threads,
// labels and search use Gmail's extensions; otherwise ids are UIDs and
// threads are rebuilt from References / In-Reply-To.
//
// One command uses one IMAP connection, opened on first use and logged out
// by Close. SMTP is only ever dialled by Send, and only in send mode.
package imap

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/mailmime"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

const Name = "imap"

// Gmail's defaults, used when the username is a gmail.com address.
const (
	GmailIMAPHost   = "imap.gmail.com"
	GmailSMTPHost   = "smtp.gmail.com"
	DefaultIMAPPort = 993
	DefaultSMTPPort = 465
)

// MaxMessageSize is what this provider writes at most; Gmail's APPENDLIMIT
// is 35 MB.
const MaxMessageSize = 35 << 20

type Config struct {
	Alias    string
	Username string // the locked address; also the login name
	Password string

	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int

	// Mode is the effective mode (policy and login). Below draft the
	// provider refuses to change anything; SMTP is used only in send mode.
	Mode policy.Mode

	TLS *tls.Config // nil: system roots (tests set their own)
}

type Provider struct {
	cfg Config
	c   *conn

	gmail bool
	boxes []mailbox
	// loc caches where a Gmail message id was found in this command.
	loc map[string]location
}

type mailbox struct {
	Name  string // decoded
	wire  string // as the server spells it (modified UTF-7)
	Attrs []string
}

type location struct {
	box string // decoded name
	uid uint32
}

func New(cfg Config) (*Provider, error) {
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errs.Auth("imap account %s has no stored credentials; run `email auth login -a %s --provider imap … --password-stdin`", cfg.Alias, cfg.Alias)
	}
	if cfg.IMAPHost == "" {
		return nil, errs.Usage("imap account %s has no IMAP host", cfg.Alias)
	}
	if cfg.IMAPPort == 0 {
		cfg.IMAPPort = DefaultIMAPPort
	}
	if cfg.SMTPPort == 0 {
		cfg.SMTPPort = DefaultSMTPPort
	}
	return &Provider{cfg: cfg, loc: map[string]location{}}, nil
}

func (p *Provider) Name() string { return Name }

// Close logs out of the IMAP connection, if one was opened.
func (p *Provider) Close() error {
	p.c.close()
	p.c = nil
	return nil
}

// conn returns the command's connection, logging in on first use.
func (p *Provider) conn(ctx context.Context) (*conn, error) {
	if p.c != nil {
		return p.c, nil
	}
	c, err := dial(ctx, p.cfg.IMAPHost, p.cfg.IMAPPort, p.cfg.TLS)
	if err != nil {
		return nil, err
	}
	if err := login(ctx, c, p.cfg.Username, p.cfg.Password); err != nil {
		c.close()
		return nil, err
	}
	p.c = c
	p.gmail = c.has("X-GM-EXT-1")
	return c, nil
}

func login(ctx context.Context, c *conn, user, pass string) error {
	if c.caps == nil {
		if err := c.capability(ctx); err != nil {
			return err
		}
	}
	if c.has("LOGINDISABLED") {
		return errs.Provider("loginDisabled", errors.New("the IMAP server does not allow LOGIN on this connection"))
	}
	rs, err := c.run(ctx, raw("LOGIN"), q(user), q(pass))
	if err != nil {
		var e *errs.Error
		// Any NO to LOGIN is a login failure: Gmail answers a normal
		// password with "NO [ALERT] Application-specific password required".
		if errors.As(err, &e) && e.Exit == errs.ExitProvider && e.ProviderCode != "BAD" && e.ProviderCode != "network" && e.ProviderCode != "bye" {
			return errs.Auth("IMAP login as %s failed (%s); check the app password, or create a new one and log in again", user, e.Message)
		}
		return err
	}
	c.caps = nil
	for _, r := range rs {
		if r.Code == "CAPABILITY" {
			c.setCaps(r.CodeArgs)
		}
	}
	if c.caps == nil {
		if err := c.capability(ctx); err != nil {
			return err
		}
	}
	if c.has("ID") {
		// RFC 2971. NetEase (163) refuses SELECT with "Unsafe Login" until
		// the client identifies itself. Failure here is not fatal.
		_, _ = c.run(ctx, raw("ID"), raw(clientID))
	}
	return nil
}

// clientID is what the ID command sends: no version, host or user data.
const clientID = `("name" "email-cli" "vendor" "james-tronai" "support-url" "https://github.com/james-tronai/email-cli")`

// ---- mailboxes ----

func (p *Provider) mailboxes(ctx context.Context) ([]mailbox, error) {
	if p.boxes != nil {
		return p.boxes, nil
	}
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	args := []any{raw("LIST"), raw(`""`), raw(`"*"`)}
	if c.has("SPECIAL-USE") && c.has("LIST-EXTENDED") {
		// Gmail sends the attributes anyway; RFC 6154 servers may want this.
		args = append(args, raw("RETURN (SPECIAL-USE)"))
	}
	rs, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	p.boxes = []mailbox{}
	for _, r := range rs {
		if r.Kind != "LIST" || len(r.Fields) < 3 {
			continue
		}
		mb := mailbox{wire: asString(r.Fields[2])}
		mb.Name = decodeMUTF7(mb.wire)
		for _, a := range asList(r.Fields[0]) {
			mb.Attrs = append(mb.Attrs, strings.ToLower(asString(a)))
		}
		if slices.Contains(mb.Attrs, `\noselect`) || slices.Contains(mb.Attrs, `\nonexistent`) {
			continue
		}
		p.boxes = append(p.boxes, mb)
	}
	return p.boxes, nil
}

// special-use attributes (RFC 6154) for the reserved labels.
var roleAttr = map[string]string{
	provider.LabelSent:    `\sent`,
	provider.LabelDrafts:  `\drafts`,
	provider.LabelTrash:   `\trash`,
	provider.LabelSpam:    `\junk`,
	provider.LabelArchive: `\archive`,
	"all":                 `\all`,
}

// byRole returns the mailbox for a reserved label ("" when the server has
// none). INBOX is always "INBOX".
func (p *Provider) byRole(ctx context.Context, role string) (string, error) {
	if role == provider.LabelInbox {
		return "INBOX", nil
	}
	boxes, err := p.mailboxes(ctx)
	if err != nil {
		return "", err
	}
	attr := roleAttr[role]
	for _, mb := range boxes {
		if slices.Contains(mb.Attrs, attr) {
			return mb.Name, nil
		}
	}
	// Servers without SPECIAL-USE: the usual names.
	for _, want := range roleNames[role] {
		for _, mb := range boxes {
			if strings.EqualFold(mb.Name, want) {
				return mb.Name, nil
			}
		}
	}
	return "", nil
}

var roleNames = map[string][]string{
	provider.LabelSent:    {"Sent", "Sent Items", "Sent Messages", "INBOX.Sent"},
	provider.LabelDrafts:  {"Drafts", "INBOX.Drafts"},
	provider.LabelTrash:   {"Trash", "Deleted Items", "Deleted Messages", "INBOX.Trash"},
	provider.LabelSpam:    {"Junk", "Spam", "Junk E-mail", "INBOX.Junk", "INBOX.Spam"},
	provider.LabelArchive: {"Archive", "Archives", "INBOX.Archive"},
}

func (p *Provider) mustRole(ctx context.Context, role string) (string, error) {
	name, err := p.byRole(ctx, role)
	if err == nil && name == "" {
		err = errs.Unsupported("the IMAP server has no %s mailbox (no special-use attribute)", role)
	}
	return name, err
}

// roleOf returns the reserved label of a mailbox, or "".
func (p *Provider) roleOf(ctx context.Context, name string) string {
	if strings.EqualFold(name, "INBOX") {
		return provider.LabelInbox
	}
	for _, role := range []string{provider.LabelDrafts, provider.LabelSent, provider.LabelTrash, provider.LabelSpam, provider.LabelArchive} {
		if b, err := p.byRole(ctx, role); err == nil && b != "" && b == name {
			return role
		}
	}
	return ""
}

// findBox returns the server's mailbox called name (case-insensitive for
// INBOX only, as IMAP has it).
func (p *Provider) findBox(ctx context.Context, name string) (string, bool, error) {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX", true, nil
	}
	boxes, err := p.mailboxes(ctx)
	if err != nil {
		return "", false, err
	}
	for _, mb := range boxes {
		if mb.Name == name {
			return mb.Name, true, nil
		}
	}
	return "", false, nil
}

// labelBox resolves a --label value to a mailbox.
func (p *Provider) labelBox(ctx context.Context, label string) (string, error) {
	low := strings.ToLower(label)
	if low == provider.LabelInbox {
		return "INBOX", nil
	}
	if _, ok := roleAttr[low]; ok && low != "all" {
		return p.mustRole(ctx, low)
	}
	name, ok, err := p.findBox(ctx, label)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errs.Usage("no mailbox or label named %q", label)
	}
	return name, nil
}

// selectBox opens a mailbox: EXAMINE (read-only, so reading never marks
// mail read) unless write is set.
func (p *Provider) selectBox(ctx context.Context, name string, write bool) (*conn, error) {
	if write && p.cfg.Mode < policy.ModeDraft {
		return nil, errs.Policy("account is in %s mode; the IMAP provider does not change mail below draft mode", p.cfg.Mode)
	}
	c, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	if c.selected == name && (!write || !c.readOnly) {
		return c, nil
	}
	cmd := "EXAMINE"
	if write {
		cmd = "SELECT"
	}
	args := []any{raw(cmd), q(encodeMUTF7(name))}
	if c.has("CONDSTORE") {
		args = append(args, raw("(CONDSTORE)"))
	}
	c.selected = ""
	rs, err := c.run(ctx, args...)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) && e.Exit == errs.ExitProvider {
			return nil, errs.Provider("selectFailed", fmt.Errorf("cannot open mailbox %q: %s", name, e.Message))
		}
		return nil, err
	}
	c.selected, c.readOnly = name, !write
	c.uidValidity, c.uidNext, c.modSeq, c.exists = 0, 0, 0, 0
	for _, r := range rs {
		switch {
		case r.Kind == "EXISTS":
			c.exists = r.Num
		case r.Code == "UIDVALIDITY" && len(r.CodeArgs) > 0:
			c.uidValidity = uint32(asUint(atom(r.CodeArgs[0])))
		case r.Code == "UIDNEXT" && len(r.CodeArgs) > 0:
			c.uidNext = uint32(asUint(atom(r.CodeArgs[0])))
		case r.Code == "HIGHESTMODSEQ" && len(r.CodeArgs) > 0:
			c.modSeq = asUint(atom(r.CodeArgs[0]))
		}
	}
	return c, nil
}

// ---- ids ----

// Gmail ids are X-GM-MSGID / X-GM-THRID in hex, as Gmail's web client and
// API spell them. Elsewhere an id is "<uidvalidity>.<uid>.<mailbox>" with
// the mailbox name base64url-encoded.

func hexID(n uint64) string { return strconv.FormatUint(n, 16) }

func parseHexID(id string) (uint64, error) {
	n, err := strconv.ParseUint(id, 16, 64)
	if err != nil || n == 0 {
		return 0, errs.Usage("%q is not a Gmail message id (hex)", id)
	}
	return n, nil
}

func uidID(box string, validity, uid uint32) string {
	return fmt.Sprintf("%d.%d.%s", validity, uid, base64.RawURLEncoding.EncodeToString([]byte(box)))
}

func parseUIDID(id string) (box string, validity, uid uint32, err error) {
	parts := strings.SplitN(id, ".", 3)
	if len(parts) == 3 {
		v, e1 := strconv.ParseUint(parts[0], 10, 32)
		u, e2 := strconv.ParseUint(parts[1], 10, 32)
		b, e3 := base64.RawURLEncoding.DecodeString(parts[2])
		if e1 == nil && e2 == nil && e3 == nil && u > 0 {
			return string(b), uint32(v), uint32(u), nil
		}
	}
	return "", 0, 0, errs.Usage("%q is not a message id of this account", id)
}

// locate finds the mailbox and UID of a message id. Gmail ids are looked
// up in All Mail first, then Drafts, Trash and Spam (which All Mail does
// not hold).
func (p *Provider) locate(ctx context.Context, id string) (location, error) {
	if l, ok := p.loc[id]; ok {
		return l, nil
	}
	if _, err := p.conn(ctx); err != nil {
		return location{}, err
	}
	if !p.gmail {
		box, validity, uid, err := parseUIDID(id)
		if err != nil {
			return location{}, err
		}
		c, err := p.selectBox(ctx, box, false)
		if err != nil {
			return location{}, err
		}
		if c.uidValidity != validity {
			return location{}, errs.NotFound("notFound", fmt.Errorf("message %s not found: mailbox %q was rebuilt on the server (UIDVALIDITY changed); list it again", id, box))
		}
		return location{box: box, uid: uid}, nil
	}
	n, err := parseHexID(id)
	if err != nil {
		return location{}, err
	}
	var boxes []string
	for _, role := range []string{"all", provider.LabelDrafts, provider.LabelTrash, provider.LabelSpam} {
		name, err := p.byRole(ctx, role)
		if err != nil {
			return location{}, err
		}
		if name != "" && !slices.Contains(boxes, name) {
			boxes = append(boxes, name)
		}
	}
	for _, box := range boxes {
		uids, err := p.search(ctx, box, raw("X-GM-MSGID"), raw(strconv.FormatUint(n, 10)))
		if err != nil {
			return location{}, err
		}
		if len(uids) > 0 {
			l := location{box: box, uid: uids[0]}
			p.loc[id] = l
			return l, nil
		}
	}
	return location{}, errs.NotFound("notFound", fmt.Errorf("message %s not found", id))
}

func (p *Provider) forget(id string) { delete(p.loc, id) }

// ---- search / fetch ----

// search runs UID SEARCH in a mailbox and returns the UIDs ascending.
func (p *Provider) search(ctx context.Context, box string, criteria ...any) ([]uint32, error) {
	c, err := p.selectBox(ctx, box, false)
	if err != nil {
		return nil, err
	}
	args := []any{raw("UID SEARCH")}
	for _, a := range criteria {
		if l, ok := a.(lit); ok && !isASCII(string(l)) {
			args = append(args, raw("CHARSET UTF-8"))
			break
		}
	}
	args = append(args, criteria...)
	rs, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, r := range rs {
		if r.Kind != "SEARCH" {
			continue
		}
		for _, f := range r.Fields {
			if n := asUint(f); n > 0 {
				out = append(out, uint32(n))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func uidSet(uids []uint32) string {
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = strconv.FormatUint(uint64(u), 10)
	}
	return strings.Join(parts, ",")
}

// headerFields is what a list fetches instead of the whole message.
const headerFields = "FROM TO CC BCC REPLY-TO SUBJECT DATE MESSAGE-ID IN-REPLY-TO REFERENCES CONTENT-TYPE"

// fetched is one message as returned by FETCH.
type fetched struct {
	box      string
	uid      uint32
	flags    []string
	date     time.Time
	size     int64
	msgID    uint64 // Gmail
	thrID    uint64 // Gmail
	labels   []string
	header   textproto.MIMEHeader
	raw      []byte // full message when fetched
	validity uint32
}

// fetch reads messages by UID; full fetches the whole message, otherwise
// only the headers a list needs. The result follows the order of uids.
func (p *Provider) fetch(ctx context.Context, box string, uids []uint32, full bool) ([]*fetched, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	c, err := p.selectBox(ctx, box, false)
	if err != nil {
		return nil, err
	}
	items := "UID FLAGS INTERNALDATE RFC822.SIZE "
	if full {
		items += "BODY.PEEK[]"
	} else {
		items += "BODY.PEEK[HEADER.FIELDS (" + headerFields + ")]"
	}
	if p.gmail {
		items += " X-GM-MSGID X-GM-THRID X-GM-LABELS"
	}
	rs, err := c.run(ctx, raw("UID FETCH"), raw(uidSet(uids)), raw("("+items+")"))
	if err != nil {
		return nil, err
	}
	byUID := map[uint32]*fetched{}
	for _, r := range rs {
		if r.Kind != "FETCH" {
			continue
		}
		it := fetchItems(r)
		f := &fetched{box: box, uid: uint32(asUint(it["UID"])), validity: c.uidValidity}
		if f.uid == 0 {
			continue
		}
		if prev := byUID[f.uid]; prev != nil {
			f = prev // a server may split one message's items over two FETCH lines
		}
		for k, v := range it {
			switch {
			case k == "FLAGS":
				f.flags = nil
				for _, x := range asList(v) {
					f.flags = append(f.flags, strings.ToLower(asString(x)))
				}
			case k == "INTERNALDATE":
				if t, err := time.Parse("_2-Jan-2006 15:04:05 -0700", asString(v)); err == nil {
					f.date = t.UTC()
				}
			case k == "RFC822.SIZE":
				f.size = int64(asUint(v))
			case k == "X-GM-MSGID":
				f.msgID = asUint(v)
			case k == "X-GM-THRID":
				f.thrID = asUint(v)
			case k == "X-GM-LABELS":
				f.labels = nil
				for _, x := range asList(v) {
					f.labels = append(f.labels, decodeMUTF7(asString(x)))
				}
			case k == "BODY[]":
				f.raw = asBytes(v)
				f.header = mailmime.Parse(f.raw).Header
			case strings.HasPrefix(k, "BODY[HEADER"):
				f.header = mailmime.ParseHeader(asBytes(v))
			}
		}
		byUID[f.uid] = f
	}
	out := make([]*fetched, 0, len(uids))
	for _, u := range uids {
		if f := byUID[u]; f != nil {
			if f.header == nil {
				f.header = textproto.MIMEHeader{}
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// fetchOne fetches the message behind an id.
func (p *Provider) fetchOne(ctx context.Context, id string, full bool) (*fetched, error) {
	l, err := p.locate(ctx, id)
	if err != nil {
		return nil, err
	}
	fs, err := p.fetch(ctx, l.box, []uint32{l.uid}, full)
	if err != nil {
		return nil, err
	}
	if len(fs) == 0 {
		p.forget(id)
		return nil, errs.NotFound("notFound", fmt.Errorf("message %s not found", id))
	}
	return fs[0], nil
}

// ---- convert ----

// Gmail web links. /u/0/ is the first signed-in Google account; Gmail
// switches accounts itself when that is another mailbox.
const webBase = "https://mail.google.com/mail/u/0/"

var gmailSystemLabels = map[string]string{
	`\inbox`: provider.LabelInbox,
	`\sent`:  provider.LabelSent,
	`\draft`: provider.LabelDrafts,
	`\trash`: provider.LabelTrash,
	`\spam`:  provider.LabelSpam,
}

// threadRoot is the generic thread id: the first message of the chain.
func threadRoot(h textproto.MIMEHeader) string {
	if refs := strings.Fields(h.Get("References")); len(refs) > 0 {
		return refs[0]
	}
	if irt := strings.Fields(h.Get("In-Reply-To")); len(irt) > 0 {
		return irt[0]
	}
	return strings.TrimSpace(h.Get("Message-Id"))
}

func (p *Provider) id(f *fetched) string {
	if p.gmail {
		return hexID(f.msgID)
	}
	return uidID(f.box, f.validity, f.uid)
}

func (p *Provider) convert(ctx context.Context, f *fetched) provider.Message {
	h := f.header
	m := provider.Message{
		ID:       p.id(f),
		Provider: Name,
		Account:  p.cfg.Alias,
		From:     mailmime.ParseAddr(h.Get("From")),
		To:       mailmime.ParseAddrList(h.Get("To")),
		Cc:       mailmime.ParseAddrList(h.Get("Cc")),
		Bcc:      mailmime.ParseAddrList(h.Get("Bcc")),
		ReplyTo:  mailmime.ParseAddrList(h.Get("Reply-To")),
		Subject:  mailmime.DecodeHeader(h.Get("Subject")),
		Date:     f.date,
		Flags: provider.Flags{
			Unread:  !slices.Contains(f.flags, `\seen`),
			Draft:   slices.Contains(f.flags, `\draft`),
			Flagged: slices.Contains(f.flags, `\flagged`),
		},
	}
	if m.Date.IsZero() {
		if t, err := mail.ParseDate(h.Get("Date")); err == nil {
			m.Date = t.UTC()
		}
	}
	boxRole := p.roleOf(ctx, f.box)
	if boxRole == provider.LabelDrafts {
		m.Flags.Draft = true
	}
	if p.gmail {
		m.ThreadID = hexID(f.thrID)
		located := false
		addLabel := func(l string) {
			if !slices.Contains(m.Labels, l) {
				m.Labels = append(m.Labels, l)
			}
		}
		if boxRole != "" {
			addLabel(boxRole)
			located = true
		}
		for _, l := range f.labels {
			low := strings.ToLower(l)
			if r, ok := gmailSystemLabels[low]; ok {
				addLabel(r)
				located = true
				continue
			}
			switch low {
			case `\starred`:
				m.Flags.Flagged = true
				continue
			case `\important`:
				continue
			}
			if !strings.HasPrefix(l, `\`) {
				addLabel(l)
			}
		}
		if !located && !m.Flags.Draft {
			m.Labels = append([]string{provider.LabelArchive}, m.Labels...)
		}
		if slices.Contains(m.Labels, provider.LabelDrafts) {
			m.Flags.Draft = true
		}
		if m.Flags.Draft {
			m.WebLink = webBase + "#drafts/" + m.ID
		} else {
			m.WebLink = webBase + "#inbox/" + m.ID
		}
	} else {
		m.ThreadID = threadRoot(h)
		if boxRole != "" {
			m.Labels = []string{boxRole}
		} else {
			m.Labels = []string{f.box}
		}
	}
	if f.raw != nil {
		pm := mailmime.Parse(f.raw)
		m.BodyText, m.BodyHTML = pm.Bodies()
		m.Attachments = pm.Attachments()
		for _, a := range m.Attachments {
			if !a.Inline {
				m.Flags.HasAttachments = true
			}
		}
	} else if ct := strings.ToLower(h.Get("Content-Type")); strings.HasPrefix(ct, "multipart/mixed") {
		m.Flags.HasAttachments = true
	}
	hdr := map[string]string{}
	for k, v := range h {
		if mailmime.HeaderAllow[strings.ToLower(k)] && len(v) > 0 {
			hdr[textproto.CanonicalMIMEHeaderKey(k)] = v[0]
		}
	}
	if len(hdr) > 0 {
		m.Headers = hdr
	}
	return m
}
