package imap

import (
	"context"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

// IDLE against go-imap's server: new mail delivered by another session
// wakes it, a quiet mailbox times out cleanly, and the connection stays
// usable for the next sync.
func TestMemIdleWakesOnNewMail(t *testing.T) {
	e := newMemEnv(t, policy.ModeRead, true)
	ctx := context.Background()
	base, err := e.p.Sync(ctx, provider.SyncOptions{})
	if err != nil || !base.Initialized {
		t.Fatalf("baseline %+v %v", base, err)
	}

	start := time.Now()
	woke, err := e.p.Idle(ctx, 150*time.Millisecond)
	if err != nil || woke || time.Since(start) > 3*time.Second {
		t.Fatalf("quiet idle: woke=%v err=%v after %v", woke, err, time.Since(start))
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		e.deliver(t, "INBOX", lunchMsg)
	}()
	start = time.Now()
	woke, err = e.p.Idle(ctx, 10*time.Second)
	if err != nil || !woke || time.Since(start) > 5*time.Second {
		t.Fatalf("idle: woke=%v err=%v after %v", woke, err, time.Since(start))
	}
	res, err := e.p.Sync(ctx, provider.SyncOptions{State: base.State})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].Subject != "Lunch on Friday?" {
		t.Fatalf("sync after idle: %+v %v", res, err)
	}
	if res.Messages[0].BodyText != "" {
		t.Fatal("sync must not read bodies")
	}
	if !res.Messages[0].Flags.Unread {
		t.Fatal("IDLE and sync must not mark mail read")
	}

	// Cancelling (SIGTERM) ends the IDLE with DONE; LOGOUT still works.
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := e.p.Idle(cctx, 10*time.Second); err != nil {
		t.Fatalf("cancelled idle: %v", err)
	}
	if e.p.c == nil || e.p.c.broken {
		t.Fatal("a clean DONE must leave the connection usable")
	}
	if _, err := e.p.Sync(ctx, provider.SyncOptions{State: res.State}); err != nil {
		t.Fatal(err)
	}
}

func TestIdleUnsupported(t *testing.T) {
	e := newMemEnv(t, policy.ModeRead, false)
	c, err := e.p.conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	delete(c.caps, "IDLE") // go-imap's server always offers it
	_, err = e.p.Idle(context.Background(), time.Second)
	if err == nil || errs.From(err).Code != "unsupported" {
		t.Fatalf("err %v", err)
	}
}

// The Gmail path: IDLE on INBOX, then a sync that returns the hex
// X-GM-MSGID, the thread and an #inbox link, read with EXAMINE only.
func TestGmailIdleThenSync(t *testing.T) {
	e := newGmailEnv(t, policy.ModeRead)
	ctx := context.Background()
	base, err := e.p.Sync(ctx, provider.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		e.f.deliver(lunchMsg)
	}()
	woke, err := e.p.Idle(ctx, 10*time.Second)
	if err != nil || !woke {
		t.Fatalf("woke=%v err=%v", woke, err)
	}
	res, err := e.p.Sync(ctx, provider.SyncOptions{State: base.State})
	if err != nil || len(res.Messages) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	m := res.Messages[0]
	if m.ID == "" || m.ThreadID == "" || m.WebLink != webBase+"#inbox/"+m.ID || m.From.Email != "alice@example.com" || !m.Flags.Unread {
		t.Fatalf("message %+v", m)
	}
	for _, c := range e.f.commands() {
		switch c {
		case "SELECT", "STORE", "UID STORE", "APPEND", "UID MOVE", "EXPUNGE", "UID EXPUNGE":
			t.Fatalf("watch must only read; sent %s (%v)", c, e.f.commands())
		}
	}
}

// Mail the server reports during another command (as an untagged EXISTS)
// is not reported again in IDLE: Idle must return at once for it.
func TestIdleReturnsForMailReportedDuringSync(t *testing.T) {
	e := newMemEnv(t, policy.ModeRead, true)
	ctx := context.Background()
	base, err := e.p.Sync(ctx, provider.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e.deliver(t, "INBOX", lunchMsg)
	if _, err := e.p.search(ctx, "INBOX", raw("ALL")); err != nil { // any command picks up the EXISTS
		t.Fatal(err)
	}
	if !e.p.c.newMail {
		t.Fatal("EXISTS during a command was not noticed")
	}
	start := time.Now()
	woke, err := e.p.Idle(ctx, 10*time.Second)
	if err != nil || !woke || time.Since(start) > time.Second {
		t.Fatalf("woke=%v err=%v after %v", woke, err, time.Since(start))
	}
	res, err := e.p.Sync(ctx, provider.SyncOptions{State: base.State})
	if err != nil || len(res.Messages) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestServerInfoAndUIDNext(t *testing.T) {
	e := newGmailEnv(t, policy.ModeRead)
	ctx := context.Background()
	info, err := e.p.ServerInfo(ctx)
	if err != nil || !info.Gmail || !info.Idle || !info.CondStore || !info.ID {
		t.Fatalf("%+v %v", info, err)
	}
	before, err := e.p.InboxUIDNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.f.deliver(lunchMsg)
	after, err := e.p.InboxUIDNext(ctx)
	if err != nil || after <= before {
		t.Fatalf("UIDNEXT %d → %d %v", before, after, err)
	}
}
