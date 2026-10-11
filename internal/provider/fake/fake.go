// Package fake is an in-memory provider for tests and for trying the CLI
// without a real mailbox. State can optionally persist to a JSON file so
// separate CLI invocations see each other's changes.
//
// It deliberately does not implement provider.Sender.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

const Name = "fake"

type storedMessage struct {
	Seq         int64                          `json:"seq"`
	Msg         provider.Message               `json:"msg"`
	Attachments map[string]provider.Attachment `json:"attachments,omitempty"`
	AttachData  map[string][]byte              `json:"attach_data,omitempty"`
	ReplyTo     string                         `json:"reply_to_id,omitempty"`
}

type state struct {
	Seq      int64                     `json:"seq"`
	Messages map[string]*storedMessage `json:"messages"`
}

type Provider struct {
	mu      sync.Mutex
	path    string // "" = memory only
	account string
	self    provider.Address
	now     func() time.Time
	st      state
}

// New returns an empty in-memory provider.
func New(account string, self provider.Address) *Provider {
	return &Provider{account: account, self: self, now: time.Now, st: state{Messages: map[string]*storedMessage{}}}
}

// Open loads (or creates) a provider persisted at path.
func Open(path, account string, self provider.Address) (*Provider, error) {
	p := New(account, self)
	p.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &p.st); err != nil {
		return nil, fmt.Errorf("fake store %s: %w", path, err)
	}
	if p.st.Messages == nil {
		p.st.Messages = map[string]*storedMessage{}
	}
	return p, nil
}

func (p *Provider) Name() string { return Name }

func (p *Provider) save() error {
	if p.path == "" {
		return nil
	}
	b, err := json.Marshal(p.st)
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// Deliver adds an incoming message to the inbox (test/seed helper).
func (p *Provider) Deliver(m provider.Message, attachments ...provider.Attachment) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.Labels == nil {
		m.Labels = []string{provider.LabelInbox}
	}
	if m.Date.IsZero() {
		m.Date = p.now().UTC()
	}
	m.Flags.Unread = true
	sm := p.insert(m)
	for _, a := range attachments {
		a.ID = "att-" + strconv.Itoa(len(sm.Attachments)+1)
		if a.Size == 0 {
			a.Size = int64(len(a.Data))
		}
		sm.Attachments[a.ID] = provider.Attachment{AttachmentMeta: a.AttachmentMeta}
		sm.AttachData[a.ID] = a.Data
		sm.Msg.Attachments = append(sm.Msg.Attachments, a.AttachmentMeta)
	}
	sm.Msg.Flags.HasAttachments = len(sm.Msg.Attachments) > 0
	out := sm.Msg
	return &out, p.save()
}

func (p *Provider) insert(m provider.Message) *storedMessage {
	p.st.Seq++
	if m.ID == "" {
		m.ID = fmt.Sprintf("fake-%04d", p.st.Seq)
	}
	if m.ThreadID == "" {
		m.ThreadID = "thread-" + m.ID
	}
	m.Provider = Name
	m.Account = p.account
	m.WebLink = "https://mail.example.test/#/" + m.ID
	if m.Snippet == "" {
		m.Snippet = snippet(m.BodyText)
	}
	sm := &storedMessage{Seq: p.st.Seq, Msg: m, Attachments: map[string]provider.Attachment{}, AttachData: map[string][]byte{}}
	p.st.Messages[m.ID] = sm
	return sm
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200])
	}
	return s
}

func notFound(id string) error {
	return errs.Provider("notFound", fmt.Errorf("message %s not found", id))
}

func (p *Provider) get(id string) (*storedMessage, error) {
	sm, ok := p.st.Messages[id]
	if !ok {
		return nil, notFound(id)
	}
	return sm, nil
}

func (p *Provider) sortedMessages(filter func(*storedMessage) bool) []*storedMessage {
	var out []*storedMessage
	for _, sm := range p.st.Messages {
		if filter(sm) {
			out = append(out, sm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out
}

func page(sms []*storedMessage, limit int, cursor string) (*provider.Page, error) {
	start := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return nil, errs.Usage("invalid cursor %q", cursor)
		}
		start = n
	}
	if limit <= 0 {
		limit = 20
	}
	pg := &provider.Page{Messages: []provider.Message{}}
	for i := start; i < len(sms) && i < start+limit; i++ {
		m := sms[i].Msg
		m.BodyText, m.BodyHTML, m.Headers = "", "", nil
		pg.Messages = append(pg.Messages, m)
	}
	if start+limit < len(sms) {
		pg.NextCursor = strconv.Itoa(start + limit)
	}
	return pg, nil
}

func hasLabel(m *provider.Message, l string) bool {
	return slices.ContainsFunc(m.Labels, func(x string) bool { return strings.EqualFold(x, l) })
}

func (p *Provider) List(_ context.Context, o provider.ListOptions) (*provider.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	label := o.Label
	if label == "" {
		label = provider.LabelInbox
	}
	return page(p.sortedMessages(func(sm *storedMessage) bool {
		return hasLabel(&sm.Msg, label) &&
			(!o.Unread || sm.Msg.Flags.Unread) &&
			(o.Since.IsZero() || !sm.Msg.Date.Before(o.Since))
	}), o.Limit, o.Cursor)
}

// Search matches every whitespace-separated term case-insensitively against
// subject, from and body.
func (p *Provider) Search(_ context.Context, o provider.SearchOptions) (*provider.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	terms := strings.Fields(strings.ToLower(o.Query))
	return page(p.sortedMessages(func(sm *storedMessage) bool {
		hay := strings.ToLower(sm.Msg.Subject + " " + sm.Msg.From.String() + " " + sm.Msg.BodyText + " " + sm.Msg.BodyHTML)
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				return false
			}
		}
		return !sm.Msg.Flags.Draft
	}), o.Limit, o.Cursor)
}

func (p *Provider) Get(_ context.Context, id string, o provider.GetOptions) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, err := p.get(id)
	if err != nil {
		return nil, err
	}
	m := sm.Msg
	if !o.Headers {
		m.Headers = nil
	}
	return &m, nil
}

func (p *Provider) Thread(_ context.Context, threadID string, limit int) ([]provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sms := p.sortedMessages(func(sm *storedMessage) bool { return sm.Msg.ThreadID == threadID })
	if len(sms) == 0 {
		return nil, errs.Provider("notFound", fmt.Errorf("thread %s not found", threadID))
	}
	slices.Reverse(sms) // oldest first
	if limit > 0 && len(sms) > limit {
		sms = sms[len(sms)-limit:]
	}
	out := make([]provider.Message, len(sms))
	for i, sm := range sms {
		out[i] = sm.Msg
		out[i].Headers = nil
	}
	return out, nil
}

func (p *Provider) ListAttachments(_ context.Context, id string) ([]provider.AttachmentMeta, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, err := p.get(id)
	if err != nil {
		return nil, err
	}
	return append([]provider.AttachmentMeta{}, sm.Msg.Attachments...), nil
}

func (p *Provider) GetAttachment(_ context.Context, id, aid string) (*provider.Attachment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, err := p.get(id)
	if err != nil {
		return nil, err
	}
	a, ok := sm.Attachments[aid]
	if !ok {
		return nil, errs.Provider("notFound", fmt.Errorf("attachment %s not found on %s", aid, id))
	}
	a.Data = sm.AttachData[aid]
	return &a, nil
}

func (p *Provider) newDraft(in provider.DraftInput, threadID, replyTo string) *storedMessage {
	m := provider.Message{
		ThreadID: threadID,
		From:     p.self,
		To:       in.To, Cc: in.Cc, Bcc: in.Bcc,
		Subject:  in.Subject,
		BodyText: in.Body,
		Date:     p.now().UTC(),
		Labels:   []string{provider.LabelDrafts},
		Flags:    provider.Flags{Draft: true},
	}
	sm := p.insert(m)
	sm.ReplyTo = replyTo
	p.attach(sm, in.Attachments)
	return sm
}

func (p *Provider) attach(sm *storedMessage, files []provider.AttachmentFile) {
	for _, f := range files {
		id := "att-" + strconv.Itoa(len(sm.Attachments)+1)
		meta := provider.AttachmentMeta{ID: id, Name: f.Name, Size: int64(len(f.Data)), MIME: f.MIME}
		sm.Attachments[id] = provider.Attachment{AttachmentMeta: meta}
		sm.AttachData[id] = f.Data
		sm.Msg.Attachments = append(sm.Msg.Attachments, meta)
	}
	sm.Msg.Flags.HasAttachments = len(sm.Msg.Attachments) > 0
}

func (p *Provider) CreateDraft(_ context.Context, in provider.DraftInput) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm := p.newDraft(in, "", "")
	out := sm.Msg
	return &out, p.save()
}

func (p *Provider) CreateReplyDraft(_ context.Context, in provider.ReplyInput) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	orig, err := p.get(in.MessageID)
	if err != nil {
		return nil, err
	}
	to := orig.Msg.ReplyTo
	if len(to) == 0 {
		to = []provider.Address{orig.Msg.From}
	}
	var cc []provider.Address
	if in.All {
		for _, a := range append(append([]provider.Address{}, orig.Msg.To...), orig.Msg.Cc...) {
			if !strings.EqualFold(a.Email, p.self.Email) {
				cc = append(cc, a)
			}
		}
	}
	subj := orig.Msg.Subject
	if !strings.HasPrefix(strings.ToLower(subj), "re:") {
		subj = "Re: " + subj
	}
	sm := p.newDraft(provider.DraftInput{To: to, Cc: cc, Subject: subj, Body: in.Body, Attachments: in.Attachments}, orig.Msg.ThreadID, orig.Msg.ID)
	out := sm.Msg
	return &out, p.save()
}

func (p *Provider) CreateForwardDraft(_ context.Context, in provider.ForwardInput) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	orig, err := p.get(in.MessageID)
	if err != nil {
		return nil, err
	}
	body := in.Body + "\n\n---------- Forwarded message ----------\nFrom: " + orig.Msg.From.String() +
		"\nSubject: " + orig.Msg.Subject + "\n\n" + orig.Msg.BodyText
	sm := p.newDraft(provider.DraftInput{To: in.To, Subject: "Fwd: " + orig.Msg.Subject, Body: body}, orig.Msg.ThreadID, orig.Msg.ID)
	for aid, a := range orig.Attachments {
		sm.Attachments[aid] = a
		sm.AttachData[aid] = orig.AttachData[aid]
		sm.Msg.Attachments = append(sm.Msg.Attachments, a.AttachmentMeta)
	}
	sm.Msg.Flags.HasAttachments = len(sm.Msg.Attachments) > 0
	out := sm.Msg
	return &out, p.save()
}

func (p *Provider) draft(id string) (*storedMessage, error) {
	sm, err := p.get(id)
	if err != nil {
		return nil, err
	}
	if !sm.Msg.Flags.Draft {
		return nil, errs.Usage("%s is not a draft", id)
	}
	return sm, nil
}

func (p *Provider) UpdateDraft(_ context.Context, id string, u provider.DraftUpdate) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, err := p.draft(id)
	if err != nil {
		return nil, err
	}
	if u.To != nil {
		sm.Msg.To = *u.To
	}
	if u.Cc != nil {
		sm.Msg.Cc = *u.Cc
	}
	if u.Bcc != nil {
		sm.Msg.Bcc = *u.Bcc
	}
	if u.Subject != nil {
		sm.Msg.Subject = *u.Subject
	}
	if u.Body != nil {
		sm.Msg.BodyText = *u.Body
		sm.Msg.Snippet = snippet(*u.Body)
	}
	p.attach(sm, u.Attachments)
	out := sm.Msg
	return &out, p.save()
}

// ReplyContext reports whether a draft was created as a reply or forward.
func (p *Provider) ReplyContext(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, ok := p.st.Messages[id]
	return ok && sm.ReplyTo != ""
}

func (p *Provider) ListDrafts(_ context.Context, o provider.ListOptions) (*provider.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return page(p.sortedMessages(func(sm *storedMessage) bool { return sm.Msg.Flags.Draft }), o.Limit, o.Cursor)
}

func (p *Provider) DeleteDraft(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.draft(id); err != nil {
		return err
	}
	delete(p.st.Messages, id)
	return p.save()
}

// folderLabels are mutually exclusive, like Outlook folders.
var folderLabels = []string{provider.LabelInbox, provider.LabelArchive, provider.LabelTrash, provider.LabelSpam}

func (p *Provider) Move(_ context.Context, id, dest string) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, err := p.get(id)
	if err != nil {
		return nil, err
	}
	dest = strings.ToLower(dest)
	sm.Msg.Labels = slices.DeleteFunc(sm.Msg.Labels, func(l string) bool { return slices.Contains(folderLabels, strings.ToLower(l)) })
	sm.Msg.Labels = append(sm.Msg.Labels, dest)
	out := sm.Msg
	return &out, p.save()
}

func (p *Provider) Relabel(_ context.Context, id string, add, remove []string) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, err := p.get(id)
	if err != nil {
		return nil, err
	}
	sm.Msg.Labels = slices.DeleteFunc(sm.Msg.Labels, func(l string) bool {
		return slices.ContainsFunc(remove, func(r string) bool { return strings.EqualFold(r, l) })
	})
	for _, a := range add {
		if !hasLabel(&sm.Msg, a) {
			sm.Msg.Labels = append(sm.Msg.Labels, a)
		}
	}
	out := sm.Msg
	return &out, p.save()
}

func (p *Provider) Mark(_ context.Context, id string, o provider.MarkOptions) (*provider.Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sm, err := p.get(id)
	if err != nil {
		return nil, err
	}
	if o.Read != nil {
		sm.Msg.Flags.Unread = !*o.Read
	}
	if o.Flagged != nil {
		sm.Msg.Flags.Flagged = *o.Flagged
	}
	out := sm.Msg
	return &out, p.save()
}

// Sync returns messages in the label with a sequence number above the state
// cursor. An empty state establishes a baseline without returning mail.
func (p *Provider) Sync(_ context.Context, o provider.SyncOptions) (*provider.SyncResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A long-running watch sees mail that another process (a CLI command,
	// a test) added since the store was opened.
	if p.path != "" {
		if b, err := os.ReadFile(p.path); err == nil {
			var st state
			if json.Unmarshal(b, &st) == nil && st.Messages != nil {
				p.st = st
			}
		}
	}
	label := o.Label
	if label == "" {
		label = provider.LabelInbox
	}
	cur := strconv.FormatInt(p.st.Seq, 10)
	if o.State == "" {
		return &provider.SyncResult{Messages: []provider.Message{}, State: cur, Initialized: true}, nil
	}
	since, err := strconv.ParseInt(o.State, 10, 64)
	if err != nil {
		return nil, errs.Usage("invalid sync state %q", o.State)
	}
	sms := p.sortedMessages(func(sm *storedMessage) bool { return sm.Seq > since && hasLabel(&sm.Msg, label) })
	slices.Reverse(sms)
	res := &provider.SyncResult{Messages: []provider.Message{}, State: cur}
	for _, sm := range sms {
		m := sm.Msg
		m.BodyText, m.BodyHTML, m.Headers = "", "", nil
		res.Messages = append(res.Messages, m)
	}
	return res, nil
}

// Raw answers GET /messages and GET /messages/{id} so the `api` escape hatch
// can be exercised; everything else is 404.
func (p *Provider) Raw(_ context.Context, r provider.RawRequest) (*provider.RawResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	path := strings.Trim(r.Path, "/")
	if r.Method == "GET" && path == "messages" {
		ids := make([]string, 0, len(p.st.Messages))
		for id := range p.st.Messages {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		b, _ := json.Marshal(map[string]any{"ids": ids})
		return &provider.RawResponse{Status: 200, Body: b}, nil
	}
	if id, ok := strings.CutPrefix(path, "messages/"); ok && r.Method == "GET" {
		if sm, ok := p.st.Messages[id]; ok {
			b, _ := json.Marshal(sm.Msg)
			return &provider.RawResponse{Status: 200, Body: b}, nil
		}
	}
	return &provider.RawResponse{Status: 404, Body: []byte(`{"error":"not found"}`)}, nil
}
