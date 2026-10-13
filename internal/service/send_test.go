package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/provider/fake"
)

// sendingFake adds provider.Sender to the fake: sending removes the draft,
// as both real providers do.
type sendingFake struct {
	*fake.Provider
	sent   []string
	bodies []string
	fail   error
}

func (f *sendingFake) Send(ctx context.Context, id string) (*provider.SendResult, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	d, err := f.Get(ctx, id, provider.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := f.DeleteDraft(ctx, id); err != nil {
		return nil, err
	}
	f.sent = append(f.sent, id)
	f.bodies = append(f.bodies, d.BodyText)
	return &provider.SendResult{MessageID: "sent-" + id, ThreadID: d.ThreadID}, nil
}

func newSendSvc(t *testing.T) (*Service, *sendingFake) {
	t.Helper()
	s, fp := newSvc(t, policy.ModeSend, policy.ModeSend)
	s.Policy.Send.AllowedRecipientDomains = []string{"tronai.com.au"}
	sf := &sendingFake{Provider: fp}
	s.Provider = sf
	return s, sf
}

// replyDraft creates a received message and a reply draft to it.
func replyDraft(t *testing.T, s *Service, fp *sendingFake, from string) string {
	t.Helper()
	m, _ := fp.Deliver(provider.Message{From: provider.Address{Email: from}, To: []provider.Address{{Email: "james@tronai.com.au"}}, Subject: "q", BodyText: "question"})
	res, err := s.ReplyDraft(ctx, provider.ReplyInput{MessageID: m.ID, Body: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	return res.(*provider.Message).ID
}

func TestSendReplyAndIdempotency(t *testing.T) {
	s, sf := newSendSvc(t)
	id := replyDraft(t, s, sf, "boss@tronai.com.au")

	res, err := s.Send(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	out := res.(*SendOutput)
	if !out.Sent || out.Replayed || out.MessageID != "sent-"+id || out.IdempotencyKey != id || strings.Join(out.Recipients, ",") != "boss@tronai.com.au" {
		t.Fatalf("output: %+v", out)
	}
	if strings.Count(sf.bodies[0], "automated assistant") != 1 {
		t.Fatalf("send must not change the body: %q", sf.bodies[0])
	}
	es := readAudit(t, s)
	last := es[len(es)-1]
	if last.Op != "send" || last.Result != "ok" || last.ResultID != "sent-"+id || last.Recipients[0] != "boss@tronai.com.au" {
		t.Fatalf("audit: %+v", last)
	}

	// A retry returns the first result and sends nothing.
	res, err = s.Send(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if again := res.(*SendOutput); !again.Replayed || again.MessageID != out.MessageID || len(sf.sent) != 1 {
		t.Fatalf("replay: %+v sent=%v", again, sf.sent)
	}
	if es := readAudit(t, s); es[len(es)-1].Result != "replayed" {
		t.Fatalf("replay audit: %+v", es[len(es)-1])
	}

	// An explicit key covers a different draft too.
	id2 := replyDraft(t, s, sf, "boss@tronai.com.au")
	if _, err := s.Send(ctx, id2, "job-42"); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Send(ctx, replyDraft(t, s, sf, "boss@tronai.com.au"), "job-42")
	if !res.(*SendOutput).Replayed || len(sf.sent) != 2 {
		t.Fatalf("explicit key: sent=%v", sf.sent)
	}
}

func TestSendPolicyRules(t *testing.T) {
	s, sf := newSendSvc(t)

	// Draft mode: denied before anything else.
	s.Policy.Default.Mode = policy.ModeDraft
	if _, err := s.Send(ctx, "whatever", ""); errs.ExitCode(err) != errs.ExitPolicy {
		t.Fatalf("draft mode: %v", err)
	}
	s.Policy.Default.Mode = policy.ModeSend

	// A new message (no reply context) with require_reply_context.
	res, _ := s.CreateDraft(ctx, provider.DraftInput{To: []provider.Address{{Email: "a@tronai.com.au"}}, Subject: "s", Body: "b"})
	newID := res.(*provider.Message).ID
	_, err := s.Send(ctx, newID, "")
	if errs.ExitCode(err) != errs.ExitPolicy || !strings.Contains(err.Error(), "replies or forwards") {
		t.Fatalf("require_reply_context: %v", err)
	}
	s.Policy.Send.RequireReplyContext = false
	if _, err := s.Send(ctx, newID, ""); err != nil {
		t.Fatalf("allowed once require_reply_context is off: %v", err)
	}
	s.Policy.Send.RequireReplyContext = true

	// Recipients outside allowed_recipient_domains (the reply goes to an external sender).
	ext := replyDraft(t, s, sf, "someone@partner.example")
	if _, err := s.Send(ctx, ext, ""); errs.ExitCode(err) != errs.ExitPolicy || !strings.Contains(err.Error(), "partner.example") {
		t.Fatalf("allowed_recipient_domains: %v", err)
	}

	// max_recipients counts to + cc + bcc.
	s.Policy.Send.MaxRecipients = 1
	id := replyDraft(t, s, sf, "boss@tronai.com.au")
	more := []provider.Address{{Email: "c1@tronai.com.au"}}
	if _, err := s.UpdateDraft(ctx, id, provider.DraftUpdate{Bcc: &more}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, id, ""); errs.ExitCode(err) != errs.ExitPolicy || !strings.Contains(err.Error(), "max_recipients") {
		t.Fatalf("max_recipients: %v", err)
	}
	if len(sf.sent) != 1 {
		t.Fatalf("denied sends went out: %v", sf.sent)
	}
	denies := 0
	for _, e := range readAudit(t, s) {
		if e.Op == "send" && e.Decision == "deny" {
			denies++
		}
	}
	if denies != 4 {
		t.Fatalf("denials audited: %d", denies)
	}
}

func TestSendNotADraftDryRunAndFailure(t *testing.T) {
	s, sf := newSendSvc(t)
	m, _ := sf.Deliver(provider.Message{From: provider.Address{Email: "boss@tronai.com.au"}, Subject: "x"})
	if _, err := s.Send(ctx, m.ID, ""); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("received message: %v", err)
	}

	id := replyDraft(t, s, sf, "boss@tronai.com.au")
	s.DryRun = true
	res, err := s.Send(ctx, id, "")
	if err != nil || !res.(*DryRunResult).DryRun || len(sf.sent) != 0 {
		t.Fatalf("dry run: %v %+v", err, res)
	}
	if _, err := os.Stat(s.idemPath(id)); !os.IsNotExist(err) {
		t.Fatal("dry run left an idempotency record")
	}
	s.DryRun = false

	sf.fail = errs.Provider("ErrorSendAsDenied", errors.New("boom"))
	if _, err := s.Send(ctx, id, ""); errs.ExitCode(err) != errs.ExitProvider {
		t.Fatalf("provider failure: %v", err)
	}
	sf.fail = nil
	if _, err := s.Send(ctx, id, ""); err != nil || len(sf.sent) != 1 {
		t.Fatalf("retry after a failed send: %v", err)
	}
}

func TestSendInProgressBlocksSameKey(t *testing.T) {
	s, sf := newSendSvc(t)
	id := replyDraft(t, s, sf, "boss@tronai.com.au")
	if err := s.claimIdem(id, timeNow()); err != nil {
		t.Fatal(err)
	}
	_, err := s.Send(ctx, id, "")
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "send_in_progress" || len(sf.sent) != 0 {
		t.Fatalf("pending key: %v", err)
	}
}

func timeNow() time.Time { return time.Now().UTC() }
