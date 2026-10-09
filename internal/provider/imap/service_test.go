package imap

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/james-tronai/email-cli/internal/audit"
	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/service"
)

// The M4 send rules, run by the service layer over the imap provider.
func TestServiceSendRulesOverGmailIMAP(t *testing.T) {
	e := newGmailEnv(t, policy.ModeSend)
	lunch := e.f.deliver(lunchMsg)
	dir := t.TempDir()
	pol := policy.Builtin() // max 5 recipients, block external forward, require reply context
	pol.Default.Mode = policy.ModeSend
	s := &service.Service{
		Account:  &auth.Account{Alias: "gmail", Provider: Name, Mode: policy.ModeSend, Username: gmUser},
		Provider: e.p, Policy: pol, Audit: audit.New(dir), StateDir: dir,
	}
	ctx := context.Background()

	// A new message (no thread with received mail) is refused.
	nd, err := s.CreateDraft(ctx, provider.DraftInput{To: []provider.Address{{Email: "a@example.com"}}, Subject: "hi", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, nd.(*provider.Message).ID, ""); errs.From(err).Exit != errs.ExitPolicy {
		t.Fatalf("send without reply context: %v", err)
	}

	r, err := s.ReplyDraft(ctx, provider.ReplyInput{MessageID: hexID(lunch.msgID), Body: "Yes"})
	if err != nil {
		t.Fatal(err)
	}
	draft := r.(*provider.Message)
	if !strings.Contains(draft.BodyText, "Sent by an automated assistant") {
		t.Fatalf("signoff missing: %q", draft.BodyText)
	}
	out, err := s.Send(ctx, draft.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	so := out.(*service.SendOutput)
	if !so.Sent || so.Replayed || so.InternetMessageID == "" || so.ThreadID != hexID(lunch.thrID) {
		t.Fatalf("send = %+v", so)
	}
	// The body went out as drafted (signoff once, not added again).
	body := string(e.smtp.sent()[0].Data)
	if strings.Count(body, "Sent by an automated assistant") != 1 {
		t.Fatalf("signoff count in sent body: %s", body)
	}
	// Retrying with the same key replays; nothing is sent again.
	out, err = s.Send(ctx, draft.ID, "")
	if err != nil || !out.(*service.SendOutput).Replayed || len(e.smtp.sent()) != 1 {
		t.Fatalf("retry = %+v, %v (smtp %d)", out, err, len(e.smtp.sent()))
	}

	// Audit: recipients and the Message-ID.
	f, err := os.Open(s.Audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var sent *audit.Entry
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var en audit.Entry
		_ = json.Unmarshal(sc.Bytes(), &en)
		if en.Op == "send" && en.Result == "ok" {
			sent = &en
		}
	}
	if sent == nil || sent.ResultID != so.InternetMessageID || strings.Join(sent.Recipients, ",") != "alice@example.com" || sent.Provider != Name {
		t.Fatalf("audit = %+v", sent)
	}
}
