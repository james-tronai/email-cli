package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/audit"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

const (
	// IdempotencyWindow is how long a sent key is remembered.
	IdempotencyWindow = 24 * time.Hour
	// pendingTimeout is how long an unfinished send blocks the same key.
	// After it, a retry goes through; if the earlier send did happen, the
	// draft no longer exists and the provider refuses to send it again.
	pendingTimeout = 10 * time.Minute
)

// SendOutput is the result of `email send`.
type SendOutput struct {
	Sent              bool      `json:"sent"`
	Replayed          bool      `json:"replayed"` // true: an earlier send with this key; nothing was sent now
	Account           string    `json:"account"`
	DraftID           string    `json:"draft_id"`
	MessageID         string    `json:"message_id,omitempty"`
	InternetMessageID string    `json:"internet_message_id,omitempty"`
	ThreadID          string    `json:"thread_id,omitempty"`
	Recipients        []string  `json:"recipients"`
	Subject           string    `json:"subject"`
	SentAt            time.Time `json:"sent_at"`
	IdempotencyKey    string    `json:"idempotency_key"`
}

// IDs makes `--format id` print the sent message's id.
func (o *SendOutput) IDs() []string {
	return []string{firstNonEmpty(o.MessageID, o.InternetMessageID, o.DraftID)}
}

type idemRecord struct {
	State   string      `json:"state"` // pending | sent
	Created time.Time   `json:"created"`
	Result  *SendOutput `json:"result,omitempty"`
}

func (s *Service) idemDir() string { return filepath.Join(s.StateDir, "send") }

func (s *Service) idemPath(key string) string {
	sum := sha256.Sum256([]byte(s.Account.Alias + "\x00" + key))
	return filepath.Join(s.idemDir(), hex.EncodeToString(sum[:16])+".json")
}

func readIdem(path string) (*idemRecord, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r idemRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, nil // torn write: treat as absent
	}
	return &r, nil
}

// lookupIdem returns the earlier result for key, or an error while a send
// with that key is still in flight.
func (s *Service) lookupIdem(key string, now time.Time) (*SendOutput, error) {
	r, err := readIdem(s.idemPath(key))
	if err != nil || r == nil {
		return nil, err
	}
	switch {
	case r.State == "sent" && r.Result != nil && now.Sub(r.Created) < IdempotencyWindow:
		out := *r.Result
		out.Replayed = true
		return &out, nil
	case r.State == "pending" && now.Sub(r.Created) < pendingTimeout:
		return nil, errs.Provider("sendInProgress", fmt.Errorf("a send with idempotency key %q started at %s and has not finished; retry after %s",
			key, r.Created.Format(time.RFC3339), r.Created.Add(pendingTimeout).Format(time.RFC3339)))
	}
	return nil, nil
}

// claimIdem writes the pending record. O_EXCL makes two concurrent sends
// with the same key race for one file; the loser gets sendInProgress.
func (s *Service) claimIdem(key string, now time.Time) error {
	if err := os.MkdirAll(s.idemDir(), 0o700); err != nil {
		return err
	}
	path := s.idemPath(key)
	if r, _ := readIdem(path); r != nil {
		if out, err := s.lookupIdem(key, now); err != nil || out != nil {
			if err == nil {
				err = errs.Provider("sendInProgress", fmt.Errorf("idempotency key %q was used by a send that just finished", key))
			}
			return err
		}
		_ = os.Remove(path) // expired record
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return errs.Provider("sendInProgress", fmt.Errorf("a send with idempotency key %q is in progress", key))
	}
	if err != nil {
		return err
	}
	b, _ := json.Marshal(idemRecord{State: "pending", Created: now})
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *Service) finishIdem(key string, out *SendOutput) error {
	b, err := json.Marshal(idemRecord{State: "sent", Created: out.SentAt, Result: out})
	if err != nil {
		return err
	}
	path := s.idemPath(key)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pruneIdem removes records older than the window.
func (s *Service) pruneIdem(now time.Time) {
	ents, err := os.ReadDir(s.idemDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.idemDir(), e.Name())
		if r, err := readIdem(path); err == nil && r != nil && now.Sub(r.Created) > IdempotencyWindow {
			_ = os.Remove(path)
		}
	}
}

// hasReplyContext reports whether the draft answers an existing message:
// its thread holds at least one message that is not a draft. Both
// providers keep a reply or forward draft in the original's thread.
func hasReplyContext(ctx context.Context, r provider.Reader, d *provider.Message) (bool, error) {
	if d.ThreadID == "" {
		return false, nil
	}
	msgs, err := r.Thread(ctx, d.ThreadID, 0)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) && e.Exit == errs.ExitProvider {
			return false, nil // no thread: a new message
		}
		return false, err
	}
	for _, m := range msgs {
		if !m.Flags.Draft {
			return true, nil
		}
	}
	return false, nil
}

// Send sends an existing draft. The body is sent as written: the signoff
// was added when the draft was written.
//
// Order matters: the mode check comes first so draft mode never touches
// the network; the idempotency lookup comes before reading the draft
// because a sent draft no longer exists; the recipient rules run on the
// draft as it is now on the server.
func (s *Service) Send(ctx context.Context, draftID, key string) (any, error) {
	if key == "" {
		key = draftID
	}
	if _, err := s.gate(policy.Request{Op: policy.OpSend, HasReplyCtx: true}, draftID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	s.pruneIdem(now)
	if out, err := s.lookupIdem(key, now); err != nil || out != nil {
		if out != nil {
			s.log(audit.Entry{Op: string(policy.OpSend), Target: draftID, Recipients: out.Recipients, Decision: "allow",
				Result: "replayed", ResultID: firstNonEmpty(out.MessageID, out.InternetMessageID)})
			return out, nil
		}
		return nil, err
	}

	sender, err := capability[provider.Sender](s, "send")
	if err != nil {
		return nil, err
	}
	reader, err := capability[provider.Reader](s, "send")
	if err != nil {
		return nil, err
	}
	d, err := reader.Get(ctx, draftID, provider.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !d.Flags.Draft {
		return nil, errs.Usage("%s is not a draft; only drafts can be sent", draftID)
	}
	rcpt := emails(d.To, d.Cc, d.Bcc)
	if len(rcpt) == 0 {
		return nil, errs.Usage("draft %s has no recipients", draftID)
	}
	replyCtx, err := hasReplyContext(ctx, reader, d)
	if err != nil {
		return nil, err
	}
	dec, err := s.gate(policy.Request{Op: policy.OpSend, Recipients: rcpt, HasReplyCtx: replyCtx}, draftID)
	if err != nil {
		return nil, err
	}
	if s.DryRun {
		return s.dryRun(policy.OpSend, dec, draftID, rcpt, map[string]any{
			"draft_id": draftID, "recipients": rcpt, "subject": d.Subject, "reply_context": replyCtx, "idempotency_key": key}), nil
	}

	if err := s.claimIdem(key, now); err != nil {
		return nil, err
	}
	res, err := sender.Send(ctx, draftID)
	if err != nil {
		_ = os.Remove(s.idemPath(key)) // nothing was sent: allow a retry
		s.done(policy.OpSend, draftID, rcpt, "", err)
		return nil, err
	}
	out := &SendOutput{
		Sent: true, Account: s.Account.Alias, DraftID: draftID,
		MessageID: res.MessageID, InternetMessageID: res.InternetMessageID, ThreadID: firstNonEmpty(res.ThreadID, d.ThreadID),
		Recipients: rcpt, Subject: d.Subject, SentAt: res.SentAt, IdempotencyKey: key,
	}
	if out.SentAt.IsZero() {
		out.SentAt = now
	}
	s.done(policy.OpSend, draftID, rcpt, firstNonEmpty(out.MessageID, out.InternetMessageID), nil)
	if err := s.finishIdem(key, out); err != nil {
		fmt.Fprintf(os.Stderr, "warning: the message was sent but the idempotency record was not saved: %v\n", err)
	}
	return out, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
