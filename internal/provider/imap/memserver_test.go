package imap

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/mailmime"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

// The non-Gmail path runs against go-imap's in-memory server: an
// independent implementation of the protocol, over STARTTLS, without
// X-GM-EXT-1, CONDSTORE or SPECIAL-USE.

const memUser = "james@example.org"

type literal struct {
	*bytes.Reader
}

func (l literal) Size() int64 { return int64(l.Len()) }

type memEnv struct {
	p    *Provider
	user *imapmemserver.User
	smtp *smtpSink
	cfg  Config
}

func newMemEnv(t *testing.T, mode policy.Mode, extensions bool) *memEnv {
	t.Helper()
	srvTLS, cliTLS := testTLS(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(memUser, "pw")
	mem.AddUser(user)
	for _, name := range []string{"INBOX", "Drafts", "Sent", "Archive", "Trash"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	caps := goimap.CapSet{goimap.CapIMAP4rev1: {}}
	if extensions {
		caps[goimap.CapUIDPlus] = struct{}{}
		caps[goimap.CapMove] = struct{}{}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:      caps,
		TLSConfig: srvTLS,
		Logger:    log.New(io.Discard, "", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := hostPort(t, ln.Addr())
	imapStartTLSPorts[port] = true
	t.Cleanup(func() { delete(imapStartTLSPorts, port); srv.Close() })
	go func() { _ = srv.Serve(ln) }()

	sink, shost, sport := startSMTP(t, srvTLS, memUser, "pw", true)
	cfg := Config{Alias: "mail", Username: memUser, Password: "pw", IMAPHost: host, IMAPPort: port,
		SMTPHost: shost, SMTPPort: sport, Mode: mode, TLS: cliTLS}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return &memEnv{p: p, user: user, smtp: sink, cfg: cfg}
}

func (e *memEnv) deliver(t *testing.T, box, raw string, flags ...goimap.Flag) {
	t.Helper()
	b := []byte(strings.ReplaceAll(raw, "\n", "\r\n"))
	if _, err := e.user.Append(box, literal{bytes.NewReader(b)}, &goimap.AppendOptions{Flags: flags}); err != nil {
		t.Fatal(err)
	}
}

const ownReply = `From: james@example.org
To: Alice <alice@example.com>
Subject: Re: Lunch on Friday?
Date: Tue, 06 Oct 2026 11:00:00 +0000
Message-ID: <reply-1@example.org>
In-Reply-To: <lunch-1@example.com>
References: <lunch-1@example.com>

Yes!
`

func TestMemListSearchGetThread(t *testing.T) {
	e := newMemEnv(t, policy.ModeRead, true)
	e.deliver(t, "INBOX", invoiceMsg, goimap.FlagSeen)
	e.deliver(t, "INBOX", lunchMsg)
	e.deliver(t, "Sent", ownReply, goimap.FlagSeen)
	ctx := context.Background()

	pg, err := e.p.List(ctx, provider.ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Messages) != 1 || pg.NextCursor == "" {
		t.Fatalf("page = %+v", pg)
	}
	m := pg.Messages[0]
	if m.Subject != "Lunch on Friday?" || !m.Flags.Unread || m.WebLink != "" || !slices.Equal(m.Labels, []string{"inbox"}) {
		t.Fatalf("message = %+v", m)
	}
	if m.ThreadID != "<lunch-1@example.com>" {
		t.Fatalf("thread id = %q", m.ThreadID)
	}
	pg2, err := e.p.List(ctx, provider.ListOptions{Cursor: pg.NextCursor})
	if err != nil || len(pg2.Messages) != 1 || pg2.Messages[0].Subject != "Invoice #1042" || pg2.NextCursor != "" {
		t.Fatalf("page 2 = %+v, %v", pg2, err)
	}

	un, err := e.p.List(ctx, provider.ListOptions{Unread: true})
	if err != nil || len(un.Messages) != 1 {
		t.Fatalf("unread = %+v, %v", un, err)
	}
	sr, err := e.p.Search(ctx, provider.SearchOptions{Query: `from:alice subject:"on Friday"`})
	if err != nil || len(sr.Messages) != 1 || sr.Messages[0].Subject != "Lunch on Friday?" {
		t.Fatalf("search = %+v, %v", sr, err)
	}
	ss, err := e.p.Search(ctx, provider.SearchOptions{Query: "in:sent Yes"})
	if err != nil || len(ss.Messages) != 1 || !slices.Equal(ss.Messages[0].Labels, []string{"sent"}) {
		t.Fatalf("search in:sent = %+v, %v", ss, err)
	}

	g, err := e.p.Get(ctx, pg2.Messages[0].ID, provider.GetOptions{})
	if err != nil || len(g.Attachments) != 1 || !strings.Contains(g.BodyHTML, "invoice") {
		t.Fatalf("get = %+v, %v", g, err)
	}
	a, err := e.p.GetAttachment(ctx, g.ID, g.Attachments[0].ID)
	if err != nil || string(a.Data) != "%PDF-1.4\n" {
		t.Fatalf("attachment = %+v, %v", a, err)
	}

	th, err := e.p.Thread(ctx, m.ThreadID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(th) != 2 || th[0].Subject != "Lunch on Friday?" || th[1].From.Email != memUser {
		t.Fatalf("thread = %+v", th)
	}
	// Reading did not mark anything read.
	again, _ := e.p.List(ctx, provider.ListOptions{Unread: true})
	if len(again.Messages) != 1 {
		t.Fatal("reading marked mail read")
	}
}

func TestMemDraftsMoveSync(t *testing.T) {
	for _, ext := range []bool{true, false} {
		name := "uidplus+move"
		if !ext {
			name = "plain"
		}
		t.Run(name, func(t *testing.T) {
			e := newMemEnv(t, policy.ModeDraft, ext)
			e.deliver(t, "INBOX", lunchMsg)
			ctx := context.Background()
			pg, err := e.p.List(ctx, provider.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			orig := pg.Messages[0]

			r, err := e.p.CreateReplyDraft(ctx, provider.ReplyInput{MessageID: orig.ID, Body: "See you"})
			if err != nil {
				t.Fatal(err)
			}
			if !r.Flags.Draft || r.ThreadID != orig.ThreadID || !slices.Equal(r.Labels, []string{"drafts"}) {
				t.Fatalf("reply = %+v", r)
			}
			subj := "Lunch moved"
			up, err := e.p.UpdateDraft(ctx, r.ID, provider.DraftUpdate{Subject: &subj})
			if err != nil {
				t.Fatal(err)
			}
			if up.ID == r.ID || up.Subject != subj || !strings.HasPrefix(up.BodyText, "See you") {
				t.Fatalf("update = %+v", up)
			}
			ds, err := e.p.ListDrafts(ctx, provider.ListOptions{})
			if err != nil || len(ds.Messages) != 1 || ds.Messages[0].ID != up.ID {
				t.Fatalf("drafts after update = %+v, %v", ds, err)
			}
			if err := e.p.DeleteDraft(ctx, orig.ID); exitOf(err) != errs.ExitUsage {
				t.Fatalf("delete a received message as draft: %v", err)
			}
			if err := e.p.DeleteDraft(ctx, up.ID); err != nil {
				t.Fatal(err)
			}
			if ds, _ = e.p.ListDrafts(ctx, provider.ListOptions{}); len(ds.Messages) != 0 {
				t.Fatalf("drafts after delete = %+v", ds.Messages)
			}

			// Sync baseline before the move: the move does not count as new mail.
			st, err := e.p.Sync(ctx, provider.SyncOptions{})
			if err != nil || !st.Initialized {
				t.Fatalf("sync baseline = %+v, %v", st, err)
			}
			mv, err := e.p.Move(ctx, orig.ID, "archive")
			if err != nil {
				if !ext {
					t.Fatalf("move (COPY fallback): %v", err)
				}
				t.Fatal(err)
			}
			if mv.ID == orig.ID || !slices.Equal(mv.Labels, []string{"archive"}) {
				t.Fatalf("moved = %+v", mv)
			}
			if _, err := e.p.Relabel(ctx, mv.ID, []string{"x"}, nil); exitOf(err) != errs.ExitUsage {
				t.Fatalf("relabel without labels: %v", err)
			}
			yes := true
			if mk, err := e.p.Mark(ctx, mv.ID, provider.MarkOptions{Flagged: &yes}); err != nil || !mk.Flags.Flagged {
				t.Fatalf("mark = %+v, %v", mk, err)
			}

			e.deliver(t, "INBOX", invoiceMsg)
			res, err := e.p.Sync(ctx, provider.SyncOptions{State: st.State})
			if err != nil || len(res.Messages) != 1 || res.Messages[0].Subject != "Invoice #1042" {
				t.Fatalf("sync = %+v, %v", res, err)
			}
			if res, err = e.p.Sync(ctx, provider.SyncOptions{State: res.State}); err != nil || len(res.Messages) != 0 {
				t.Fatalf("second sync = %+v, %v", res, err)
			}

			// A mailbox rebuilt with a new UIDVALIDITY: new baseline.
			arch, err := e.p.Sync(ctx, provider.SyncOptions{Label: "archive"})
			if err != nil {
				t.Fatal(err)
			}
			e.p.Close()
			if err := e.user.Delete("Archive"); err != nil {
				t.Fatal(err)
			}
			if err := e.user.Create("Archive", nil); err != nil {
				t.Fatal(err)
			}
			e.p, _ = New(e.cfg)
			res, err = e.p.Sync(ctx, provider.SyncOptions{Label: "archive", State: arch.State})
			if err != nil || !res.Initialized {
				t.Fatalf("sync after UIDVALIDITY change = %+v, %v", res, err)
			}
			if _, err := e.p.Get(ctx, mv.ID, provider.GetOptions{}); exitOf(err) != errs.ExitProvider {
				t.Fatalf("stale id after UIDVALIDITY change: %v", err)
			}
		})
	}
}

func TestMemSendOverStartTLS(t *testing.T) {
	e := newMemEnv(t, policy.ModeSend, true)
	e.deliver(t, "INBOX", lunchMsg)
	ctx := context.Background()
	pg, _ := e.p.List(ctx, provider.ListOptions{})
	d, err := e.p.CreateReplyDraft(ctx, provider.ReplyInput{MessageID: pg.Messages[0].ID, Body: "Yes"})
	if err != nil {
		t.Fatal(err)
	}
	bcc := []provider.Address{{Email: "audit@example.org"}}
	if d, err = e.p.UpdateDraft(ctx, d.ID, provider.DraftUpdate{Bcc: &bcc}); err != nil {
		t.Fatal(err)
	}
	res, err := e.p.Send(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	sent := e.smtp.sent()
	if len(sent) != 1 || !slices.Equal(sent[0].Rcpt, []string{"alice@example.com", "audit@example.org"}) {
		t.Fatalf("smtp = %+v", sent)
	}
	if h := mailmime.Parse(sent[0].Data).Header; h.Get("Bcc") != "" || h.Get("Message-Id") != res.InternetMessageID {
		t.Fatalf("sent header = %v", h)
	}
	if ds, _ := e.p.ListDrafts(ctx, provider.ListOptions{}); len(ds.Messages) != 0 {
		t.Fatalf("draft left after send: %+v", ds.Messages)
	}
	// Not Gmail: the client saves the sent copy (without Bcc).
	s, err := e.p.List(ctx, provider.ListOptions{Label: "sent"})
	if err != nil || len(s.Messages) != 1 || s.Messages[0].Subject != "Re: Lunch on Friday?" || len(s.Messages[0].Bcc) != 0 {
		t.Fatalf("sent folder = %+v, %v", s, err)
	}
	if res.ThreadID != "<lunch-1@example.com>" {
		t.Fatalf("thread = %q", res.ThreadID)
	}
}
