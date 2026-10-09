package imap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/audit"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/watch"
)

// watchSource adapts the provider as the CLI does.
type watchSource struct{ p *Provider }

func (s watchSource) Sync(ctx context.Context, state string) (*provider.SyncResult, error) {
	return s.p.Sync(ctx, provider.SyncOptions{Label: provider.LabelInbox, State: state})
}
func (s watchSource) Close() error { return s.p.Close() }
func (s watchSource) Idle(ctx context.Context, d time.Duration) (bool, error) {
	return s.p.Idle(ctx, d)
}

func watchMail(from, subject string, n int) string {
	return fmt.Sprintf("From: %s\nTo: james@gmail.com\nSubject: %s\nDate: Fri, 09 Oct 2026 15:22:%02d +0000\nMessage-ID: <w%d@example.com>\n\nbody %d\n",
		from, subject, n, n, n)
}

// The acceptance run against the Gmail model: one message → one POST
// within seconds; three in a burst → one POST; noreply@ is filtered. The
// webhook sees the token header and metadata only.
func TestWatchOverGmailIdle(t *testing.T) {
	e := newGmailEnv(t, policy.ModeRead)
	type post struct {
		token string
		p     watch.Payload
		raw   string
		at    time.Time
	}
	var mu sync.Mutex
	var posts []post
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p watch.Payload
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		posts = append(posts, post{r.Header.Get("X-Gitlab-Token"), p, string(b), time.Now()})
		mu.Unlock()
	}))
	defer hook.Close()
	got := func() []post { mu.Lock(); defer mu.Unlock(); return append([]post(nil), posts...) }

	dir := t.TempDir()
	w, err := watch.New(watch.Config{
		Accounts: []watch.Account{{Alias: "gmail", Provider: Name, Open: func(context.Context) (watch.Source, error) {
			cfg := e.cfg
			cfg.Mode = policy.ModeRead
			p, err := New(cfg)
			return watchSource{p}, err
		}}},
		Filter:       policy.WatchSection{IgnoreSenders: []string{"noreply@"}, OnlyUnread: true},
		Notifier:     &watch.HTTPNotifier{URL: hook.URL + "/webhooks/email", Token: "tok"},
		Debounce:     400 * time.Millisecond,
		PollInterval: time.Minute, // IDLE must do the waking
		StateDir:     dir,
		Audit:        audit.New(dir),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(got()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("waited for %d POSTs, got %d", n, len(got()))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Let the baseline sync and the first IDLE start.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := w.Health()
		b, _ := json.Marshal(body)
		var h struct {
			Accounts []struct{ State, Wait string }
		}
		_ = json.Unmarshal(b, &h)
		if h.Accounts[0].State == "online" && h.Accounts[0].Wait == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not idling: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	sent := time.Now()
	e.f.deliver(watchMail("Alice <alice@example.com>", "one", 1))
	wait(1)
	p0 := got()[0]
	if p0.token != "tok" || p0.p.Count != 1 || p0.p.Messages[0].Subject != "one" || p0.at.Sub(sent) > 3*time.Second {
		t.Fatalf("first POST %+v after %v", p0, p0.at.Sub(sent))
	}

	e.f.deliver(watchMail("noreply@github.com", "ci", 2))
	for i := 3; i <= 5; i++ {
		e.f.deliver(watchMail("Bob <bob@example.com>", fmt.Sprintf("burst %d", i), i))
		time.Sleep(50 * time.Millisecond)
	}
	wait(2)
	time.Sleep(600 * time.Millisecond)
	ps := got()
	if len(ps) != 2 || ps[1].p.Count != 3 {
		t.Fatalf("burst: %d POSTs, last %+v", len(ps), ps[len(ps)-1].p)
	}
	for _, m := range ps[1].p.Messages {
		if m.From.Email == "noreply@github.com" {
			t.Fatal("noreply@ was not filtered")
		}
	}
	for _, p := range ps {
		if strings.Contains(p.raw, "body ") {
			t.Fatalf("a body reached the webhook: %s", p.raw)
		}
	}
	for _, c := range e.f.commands() {
		switch c {
		case "SELECT", "STORE", "UID STORE", "APPEND", "UID MOVE", "UID EXPUNGE", "EXPUNGE":
			t.Fatalf("watch changed the mailbox: %s", c)
		}
	}
}
