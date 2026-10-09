// Package gmail implements the provider interfaces on the Gmail REST API
// (Google Workspace accounts), with OAuth auth code + PKCE login.
package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

type Config struct {
	Alias     string
	Username  string // locked mailbox address; used as From on drafts
	BaseURL   string // default DefaultBaseURL
	HTTP      *http.Client
	Tokens    TokenSource
	UserAgent string
}

type Provider struct {
	c        *client
	alias    string
	username string

	labelsMu sync.Mutex
	labels   []gLabel // nil until loaded
}

func New(cfg Config) (*Provider, error) {
	c, err := newClient(firstNonEmpty(cfg.BaseURL, DefaultBaseURL), cfg.HTTP, cfg.Tokens, cfg.UserAgent, cfg.Alias)
	if err != nil {
		return nil, err
	}
	return &Provider{c: c, alias: cfg.Alias, username: cfg.Username}, nil
}

func (p *Provider) Name() string { return Name }

// ---- labels ----

type gLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // system | user
}

func (p *Provider) loadLabels(ctx context.Context, refresh bool) ([]gLabel, error) {
	p.labelsMu.Lock()
	defer p.labelsMu.Unlock()
	if p.labels != nil && !refresh {
		return p.labels, nil
	}
	var res struct {
		Labels []gLabel `json:"labels"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/labels", nil, &res); err != nil {
		return nil, err
	}
	if res.Labels == nil {
		res.Labels = []gLabel{}
	}
	p.labels = res.Labels
	return p.labels, nil
}

// labelNames maps user label ids to names. Failure to load labels only
// costs the names, so it is not fatal.
func (p *Provider) labelNames(ctx context.Context) map[string]string {
	ls, err := p.loadLabels(ctx, false)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, l := range ls {
		if l.Type == "user" {
			out[l.ID] = l.Name
		}
	}
	return out
}

// labelID resolves a reserved name, a user label name (case-insensitive) or
// a label id. ok is false when nothing matches.
func (p *Provider) labelID(ctx context.Context, name string) (string, bool, error) {
	if id, ok := reservedToSystem[strings.ToLower(name)]; ok {
		return id, true, nil
	}
	ls, err := p.loadLabels(ctx, false)
	if err != nil {
		return "", false, err
	}
	for _, l := range ls {
		if l.ID == name || strings.EqualFold(l.Name, name) {
			return l.ID, true, nil
		}
	}
	return "", false, nil
}

// ---- list / search ----

// cursor is a self-contained next_cursor: Gmail's pageToken is only valid
// with the same query, so the query travels with it.
type cursor struct {
	Q         string   `json:"q,omitempty"`
	LabelIDs  []string `json:"l,omitempty"`
	SpamTrash bool     `json:"st,omitempty"`
	Drafts    bool     `json:"d,omitempty"` // drafts.list, not messages.list
	Max       int      `json:"n"`
	Token     string   `json:"t"`
}

const cursorPrefix = "gm1."

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	raw, ok := strings.CutPrefix(s, cursorPrefix)
	if !ok {
		return c, errs.Usage("--cursor is not a gmail cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Token == "" {
		return c, errs.Usage("--cursor is malformed")
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

// List lists a label newest first.
func (p *Provider) List(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	if o.Cursor != "" {
		c, err := decodeCursor(o.Cursor)
		if err != nil {
			return nil, err
		}
		return p.listPage(ctx, c)
	}
	label := firstNonEmpty(o.Label, provider.LabelInbox)
	c := cursor{Max: limitOr(o.Limit, 20)}
	var q []string
	switch strings.ToLower(label) {
	case provider.LabelArchive:
		q = append(q, archiveQuery)
	case provider.LabelTrash, provider.LabelSpam:
		c.SpamTrash = true
		c.LabelIDs = append(c.LabelIDs, reservedToSystem[strings.ToLower(label)])
	default:
		id, ok, err := p.labelID(ctx, label)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errs.Usage("no Gmail label named %q", label)
		}
		c.LabelIDs = append(c.LabelIDs, id)
	}
	if o.Unread {
		c.LabelIDs = append(c.LabelIDs, "UNREAD")
	}
	if !o.Since.IsZero() {
		q = append(q, "after:"+strconv.FormatInt(o.Since.Unix()-1, 10))
	}
	c.Q = strings.Join(q, " ")
	return p.listPage(ctx, c)
}

// Search passes the query to Gmail's q= unchanged (same syntax as the
// Gmail search box). Spam and trash are excluded unless the query asks
// for them with in:spam / in:trash.
func (p *Provider) Search(ctx context.Context, o provider.SearchOptions) (*provider.Page, error) {
	if o.Cursor != "" {
		c, err := decodeCursor(o.Cursor)
		if err != nil {
			return nil, err
		}
		return p.listPage(ctx, c)
	}
	if strings.TrimSpace(o.Query) == "" {
		return nil, errs.Usage("search query is empty")
	}
	return p.listPage(ctx, cursor{Q: o.Query, Max: limitOr(o.Limit, 20)})
}

type gRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

func (p *Provider) listPage(ctx context.Context, c cursor) (*provider.Page, error) {
	if c.Drafts {
		return nil, errs.Usage("--cursor is from draft list; use it with `draft list`")
	}
	v := url.Values{}
	v.Set("maxResults", strconv.Itoa(c.Max))
	if c.Q != "" {
		v.Set("q", c.Q)
	}
	for _, l := range c.LabelIDs {
		v.Add("labelIds", l)
	}
	if c.SpamTrash {
		v.Set("includeSpamTrash", "true")
	}
	if c.Token != "" {
		v.Set("pageToken", c.Token)
	}
	var res struct {
		Messages      []gRef `json:"messages"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/messages?"+v.Encode(), nil, &res); err != nil {
		return nil, err
	}
	ids := make([]string, len(res.Messages))
	for i, r := range res.Messages {
		ids[i] = r.ID
	}
	msgs, err := p.getMetadata(ctx, ids)
	if err != nil {
		return nil, err
	}
	pg := &provider.Page{Messages: msgs}
	if res.NextPageToken != "" {
		c.Token = res.NextPageToken
		pg.NextCursor = c.encode()
	}
	return pg, nil
}

var metadataHeaders = []string{"From", "To", "Cc", "Reply-To", "Subject", "Date"}

func metadataQuery() string {
	v := url.Values{}
	v.Set("format", "metadata")
	for _, h := range metadataHeaders {
		v.Add("metadataHeaders", h)
	}
	return v.Encode()
}

const fetchConcurrency = 8

// getMetadata fetches headers for ids concurrently, keeping order. Messages
// deleted between list and get are skipped.
func (p *Provider) getMetadata(ctx context.Context, ids []string) ([]provider.Message, error) {
	gs := make([]*gMessage, len(ids))
	errsOut := make([]error, len(ids))
	sem := make(chan struct{}, fetchConcurrency)
	var wg sync.WaitGroup
	q := metadataQuery()
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			var g gMessage
			if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/messages/"+pathEsc(id)+"?"+q, nil, &g); err != nil {
				errsOut[i] = err
				return
			}
			gs[i] = &g
		}(i, id)
	}
	wg.Wait()
	names := p.labelNames(ctx)
	out := make([]provider.Message, 0, len(ids))
	for i := range ids {
		if errsOut[i] != nil {
			if isNotFound(errsOut[i]) {
				continue
			}
			return nil, errsOut[i]
		}
		m := p.convert(gs[i], names)
		m.Headers = nil
		out = append(out, m)
	}
	return out, nil
}

// ---- get / thread ----

// isDraftID tells a draft id ("r-123…", "r123…") from a message id (hex).
func isDraftID(id string) bool { return strings.HasPrefix(id, "r") }

func (p *Provider) getFull(ctx context.Context, id string) (*gMessage, error) {
	var g gMessage
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/messages/"+pathEsc(id)+"?format=full", nil, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func (p *Provider) getDraft(ctx context.Context, draftID, format string) (*gDraft, error) {
	var d gDraft
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/drafts/"+pathEsc(draftID)+"?format="+format, nil, &d); err != nil {
		return nil, err
	}
	if d.Message == nil {
		return nil, errs.Provider("invalidResponse", fmt.Errorf("draft %s has no message", draftID))
	}
	return &d, nil
}

// convertDraft maps a draft: the id is the draft id (what draft update,
// draft delete and send take) and the link opens the compose window.
func (p *Provider) convertDraft(d *gDraft, names map[string]string) provider.Message {
	m := p.convert(d.Message, names)
	m.ID = d.ID
	m.WebLink = draftLink(d.Message.ID)
	m.Flags.Draft = true
	return m
}

// Get fetches one message. A draft id ("r…") fetches the draft. HTML is
// returned as HTML; the service derives text from it locally so hidden
// text is stripped.
func (p *Provider) Get(ctx context.Context, id string, o provider.GetOptions) (*provider.Message, error) {
	var m provider.Message
	if isDraftID(id) {
		d, err := p.getDraft(ctx, id, "full")
		if err != nil {
			return nil, err
		}
		m = p.convertDraft(d, p.labelNames(ctx))
	} else {
		g, err := p.getFull(ctx, id)
		if err != nil {
			return nil, err
		}
		m = p.convert(g, p.labelNames(ctx))
	}
	if !o.Headers {
		m.Headers = nil
	}
	return &m, nil
}

const maxThreadMessages = 200

// Thread returns a thread oldest first.
func (p *Provider) Thread(ctx context.Context, threadID string, limit int) ([]provider.Message, error) {
	var t struct {
		ID       string     `json:"id"`
		Messages []gMessage `json:"messages"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/threads/"+pathEsc(threadID)+"?format=full", nil, &t); err != nil {
		return nil, err
	}
	names := p.labelNames(ctx)
	out := make([]provider.Message, 0, len(t.Messages))
	for i := range t.Messages {
		m := p.convert(&t.Messages[i], names)
		m.Headers = nil
		out = append(out, m)
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

// messageForAttachments returns the full message behind a message or draft id.
func (p *Provider) messageForAttachments(ctx context.Context, id string) (*gMessage, error) {
	if isDraftID(id) {
		d, err := p.getDraft(ctx, id, "full")
		if err != nil {
			return nil, err
		}
		return d.Message, nil
	}
	return p.getFull(ctx, id)
}

func (p *Provider) ListAttachments(ctx context.Context, id string) ([]provider.AttachmentMeta, error) {
	g, err := p.messageForAttachments(ctx, id)
	if err != nil {
		return nil, err
	}
	return attachments(g.Payload), nil
}

// GetAttachment fetches an attachment by MIME part id.
func (p *Provider) GetAttachment(ctx context.Context, id, partID string) (*provider.Attachment, error) {
	g, err := p.messageForAttachments(ctx, id)
	if err != nil {
		return nil, err
	}
	part := findPart(g.Payload, partID)
	if part == nil || !isAttachment(part) {
		return nil, errs.Provider("notFound", fmt.Errorf("attachment %s not found on %s", partID, id))
	}
	data, err := p.partData(ctx, g.ID, part)
	if err != nil {
		return nil, err
	}
	return &provider.Attachment{
		AttachmentMeta: provider.AttachmentMeta{ID: part.PartID, Name: part.Filename, Size: int64(len(data)),
			MIME: strings.ToLower(part.MimeType), Inline: isInline(part)},
		Data: data,
	}, nil
}

// partData returns a part's bytes, fetching them when Gmail stored the body
// separately (attachmentId).
func (p *Provider) partData(ctx context.Context, messageID string, part *gPart) ([]byte, error) {
	if part.Body.AttachmentID == "" {
		return decodeData(part.Body.Data), nil
	}
	var b gBody
	path := userPath + "/messages/" + pathEsc(messageID) + "/attachments/" + pathEsc(part.Body.AttachmentID)
	if err := p.c.doJSON(ctx, http.MethodGet, path, nil, &b); err != nil {
		return nil, err
	}
	return decodeData(b.Data), nil
}
