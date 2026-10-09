package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

// WithSignoff appends the policy signoff to agent-written body text, once.
func (s *Service) WithSignoff(body string) string {
	so := s.Policy.Signoff()
	if strings.TrimSpace(so) == "" || strings.Contains(body, strings.TrimSpace(so)) {
		return body
	}
	return strings.TrimRight(body, "\n") + so
}

func (s *Service) drafter() (provider.Drafter, error) {
	return capability[provider.Drafter](s, "draft")
}

func (s *Service) CreateDraft(ctx context.Context, in provider.DraftInput) (any, error) {
	rcpt := emails(in.To, in.Cc, in.Bcc)
	if len(rcpt) == 0 {
		return nil, errs.Usage("at least one recipient (--to/--cc/--bcc) is required")
	}
	d, err := s.gate(policy.Request{Op: policy.OpDraftCreate, Recipients: rcpt}, "")
	if err != nil {
		return nil, err
	}
	dr, err := s.drafter()
	if err != nil {
		return nil, err
	}
	in.Body = s.WithSignoff(in.Body)
	if s.DryRun {
		return s.dryRun(policy.OpDraftCreate, d, "", rcpt, draftArgs(in)), nil
	}
	m, err := dr.CreateDraft(ctx, in)
	s.done(policy.OpDraftCreate, "", rcpt, msgID(m), err)
	return normalized(m, err)
}

func (s *Service) ReplyDraft(ctx context.Context, in provider.ReplyInput) (any, error) {
	d, err := s.gate(policy.Request{Op: policy.OpDraftReply}, in.MessageID)
	if err != nil {
		return nil, err
	}
	dr, err := s.drafter()
	if err != nil {
		return nil, err
	}
	in.Body = s.WithSignoff(in.Body)
	if s.DryRun {
		return s.dryRun(policy.OpDraftReply, d, in.MessageID, nil, map[string]any{"message_id": in.MessageID, "all": in.All, "body": in.Body}), nil
	}
	m, err := dr.CreateReplyDraft(ctx, in)
	var rcpt []string
	if m != nil {
		rcpt = emails(m.To, m.Cc, m.Bcc)
	}
	s.done(policy.OpDraftReply, in.MessageID, rcpt, msgID(m), err)
	return normalized(m, err)
}

func (s *Service) ForwardDraft(ctx context.Context, in provider.ForwardInput) (any, error) {
	rcpt := emails(in.To)
	if len(rcpt) == 0 {
		return nil, errs.Usage("--to is required")
	}
	d, err := s.gate(policy.Request{Op: policy.OpDraftForward, Recipients: rcpt}, in.MessageID)
	if err != nil {
		return nil, err
	}
	dr, err := s.drafter()
	if err != nil {
		return nil, err
	}
	in.Body = s.WithSignoff(in.Body)
	if s.DryRun {
		return s.dryRun(policy.OpDraftForward, d, in.MessageID, rcpt, map[string]any{"message_id": in.MessageID, "to": rcpt, "body": in.Body}), nil
	}
	m, err := dr.CreateForwardDraft(ctx, in)
	s.done(policy.OpDraftForward, in.MessageID, rcpt, msgID(m), err)
	return normalized(m, err)
}

func (s *Service) UpdateDraft(ctx context.Context, id string, u provider.DraftUpdate) (any, error) {
	var rcpt []string
	for _, l := range []*[]provider.Address{u.To, u.Cc, u.Bcc} {
		if l != nil {
			rcpt = append(rcpt, emails(*l)...)
		}
	}
	d, err := s.gate(policy.Request{Op: policy.OpDraftUpdate, Recipients: rcpt}, id)
	if err != nil {
		return nil, err
	}
	dr, err := s.drafter()
	if err != nil {
		return nil, err
	}
	if u.Body != nil {
		b := s.WithSignoff(*u.Body)
		u.Body = &b
	}
	if s.DryRun {
		return s.dryRun(policy.OpDraftUpdate, d, id, rcpt, u), nil
	}
	m, err := dr.UpdateDraft(ctx, id, u)
	s.done(policy.OpDraftUpdate, id, rcpt, msgID(m), err)
	return normalized(m, err)
}

func (s *Service) ListDrafts(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	if _, err := s.gate(policy.Request{Op: policy.OpDraftList}, ""); err != nil {
		return nil, err
	}
	dr, err := s.drafter()
	if err != nil {
		return nil, err
	}
	return dr.ListDrafts(ctx, o)
}

type OKResult struct {
	OK bool   `json:"ok"`
	ID string `json:"id"`
}

func (s *Service) DeleteDraft(ctx context.Context, id string) (any, error) {
	d, err := s.gate(policy.Request{Op: policy.OpDraftDelete}, id)
	if err != nil {
		return nil, err
	}
	dr, err := s.drafter()
	if err != nil {
		return nil, err
	}
	if s.DryRun {
		return s.dryRun(policy.OpDraftDelete, d, id, nil, map[string]string{"id": id}), nil
	}
	err = dr.DeleteDraft(ctx, id)
	s.done(policy.OpDraftDelete, id, nil, "", err)
	if err != nil {
		return nil, err
	}
	return &OKResult{OK: true, ID: id}, nil
}

// ---- move / label / mark ----

func (s *Service) Move(ctx context.Context, id, dest string) (any, error) {
	if dest == "" {
		return nil, errs.Usage("--to is required")
	}
	d, err := s.gate(policy.Request{Op: policy.OpMove, Dest: dest}, id)
	if err != nil {
		return nil, err
	}
	mv, err := capability[provider.Mover](s, "move")
	if err != nil {
		return nil, err
	}
	if s.DryRun {
		return s.dryRun(policy.OpMove, d, id, nil, map[string]string{"id": id, "to": dest}), nil
	}
	m, err := mv.Move(ctx, id, dest)
	s.done(policy.OpMove, id, nil, dest, err)
	return normalized(m, err)
}

func (s *Service) Label(ctx context.Context, id string, add, remove []string) (any, error) {
	if len(add)+len(remove) == 0 {
		return nil, errs.Usage("--add or --remove is required")
	}
	d, err := s.gate(policy.Request{Op: policy.OpLabel, AddLabels: add}, id)
	if err != nil {
		return nil, err
	}
	mv, err := capability[provider.Mover](s, "label")
	if err != nil {
		return nil, err
	}
	if s.DryRun {
		return s.dryRun(policy.OpLabel, d, id, nil, map[string]any{"id": id, "add": add, "remove": remove}), nil
	}
	m, err := mv.Relabel(ctx, id, add, remove)
	s.done(policy.OpLabel, id, nil, "", err)
	return normalized(m, err)
}

func (s *Service) Mark(ctx context.Context, id string, o provider.MarkOptions) (any, error) {
	if o.Read == nil && o.Flagged == nil {
		return nil, errs.Usage("one of --read, --unread, --flag, --unflag is required")
	}
	d, err := s.gate(policy.Request{Op: policy.OpMark}, id)
	if err != nil {
		return nil, err
	}
	mv, err := capability[provider.Mover](s, "mark")
	if err != nil {
		return nil, err
	}
	if s.DryRun {
		return s.dryRun(policy.OpMark, d, id, nil, o), nil
	}
	m, err := mv.Mark(ctx, id, o)
	s.done(policy.OpMark, id, nil, "", err)
	return normalized(m, err)
}

// ---- sync ----

type SyncOutput struct {
	Account     string             `json:"account"`
	Label       string             `json:"label"`
	Initialized bool               `json:"initialized"`
	StateFile   string             `json:"state_file"`
	Messages    []provider.Message `json:"messages"`
}

func (o *SyncOutput) IDs() []string {
	ids := make([]string, len(o.Messages))
	for i, m := range o.Messages {
		ids[i] = m.ID
	}
	return ids
}

var unsafeFileChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (s *Service) DefaultSyncStateFile(label string) string {
	return filepath.Join(s.StateDir, "sync", s.Account.Alias+"-"+unsafeFileChars.ReplaceAllString(label, "_")+".cursor")
}

// Sync returns mail that arrived since the cursor in stateFile, then advances
// the cursor. The first run only records a baseline.
func (s *Service) Sync(ctx context.Context, label, stateFile string) (*SyncOutput, error) {
	if label == "" {
		label = provider.LabelInbox
	}
	if stateFile == "" {
		stateFile = s.DefaultSyncStateFile(label)
	}
	if _, err := s.gate(policy.Request{Op: policy.OpSync}, label); err != nil {
		return nil, err
	}
	sy, err := capability[provider.Syncer](s, "sync")
	if err != nil {
		return nil, err
	}
	prev, err := os.ReadFile(stateFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	res, err := sy.Sync(ctx, provider.SyncOptions{Label: label, State: strings.TrimSpace(string(prev))})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o700); err != nil {
		return nil, err
	}
	tmp := stateFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(res.State+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, stateFile); err != nil {
		return nil, err
	}
	msgs := res.Messages
	if msgs == nil {
		msgs = []provider.Message{}
	}
	return &SyncOutput{Account: s.Account.Alias, Label: label, Initialized: res.Initialized, StateFile: stateFile, Messages: msgs}, nil
}

// normalized cleans a message returned by a write the same way as `get`
// (HTML → sanitized text, no raw HTML, truncation), so a reply draft's
// quoted HTML never reaches the caller unsanitized.
func normalized(m *provider.Message, err error) (any, error) {
	if err != nil || m == nil {
		return nil, err
	}
	NormalizeBody(m, false, DefaultMaxBody)
	return m, nil
}

func msgID(m *provider.Message) string {
	if m == nil {
		return ""
	}
	return m.ID
}

func draftArgs(in provider.DraftInput) map[string]any {
	names := make([]string, len(in.Attachments))
	for i, a := range in.Attachments {
		names[i] = a.Name
	}
	return map[string]any{"to": emails(in.To), "cc": emails(in.Cc), "bcc": emails(in.Bcc), "subject": in.Subject, "body": in.Body, "attachments": names}
}
