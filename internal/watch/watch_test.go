package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/audit"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

// box is a fake mailbox: the cursor is the number of messages seen.
type box struct {
	mu      sync.Mutex
	msgs    []provider.Message
	arrived chan struct{} // wakes an Idle
	openErr error
	idleErr error // returned by Idle (e.g. unsupported)
	opens   atomic.Int32
	closes  atomic.Int32
}

func newBox() *box { return &box{arrived: make(chan struct{}, 100)} }

func (b *box) deliver(ms ...provider.Message) {
	b.mu.Lock()
	b.msgs = append(b.msgs, ms...)
	b.mu.Unlock()
	b.arrived <- struct{}{}
}

func (b *box) open(context.Context) (Source, error) {
	b.opens.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openErr != nil {
		return nil, b.openErr
	}
	return &boxSource{b}, nil
}

type boxSource struct{ b *box }

func (s *boxSource) Sync(_ context.Context, state string) (*provider.SyncResult, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	n := len(s.b.msgs)
	if state == "" {
		return &provider.SyncResult{Messages: []provider.Message{}, State: strconv.Itoa(n), Initialized: true}, nil
	}
	since, err := strconv.Atoi(state)
	if err != nil {
		return nil, errs.Usage("bad cursor")
	}
	return &provider.SyncResult{Messages: slices.Clone(s.b.msgs[min(since, n):]), State: strconv.Itoa(n)}, nil
}

func (s *boxSource) Close() error { s.b.closes.Add(1); return nil }

func (s *boxSource) Idle(ctx context.Context, maxWait time.Duration) (bool, error) {
	s.b.mu.Lock()
	ierr := s.b.idleErr
	s.b.mu.Unlock()
	if ierr != nil {
		return false, ierr
	}
	select {
	case <-s.b.arrived:
		return true, nil
	case <-time.After(maxWait):
		return false, nil
	case <-ctx.Done():
		return false, nil
	}
}

// recorder is a Notifier that keeps what it got.
type recorder struct {
	mu   sync.Mutex
	got  []Payload
	fail error
}

func (r *recorder) Notify(_ context.Context, p Payload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, p)
	return r.fail
}

func (r *recorder) payloads() []Payload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

func msg(id, from, subject string, unread bool) provider.Message {
	return provider.Message{ID: id, ThreadID: "t-" + id, From: provider.Address{Name: "N " + id, Email: from},
		Subject: subject, Date: time.Date(2026, 10, 9, 15, 22, 49, 0, time.UTC), Flags: provider.Flags{Unread: unread},
		WebLink: "https://mail.example/" + id}
}

type harness struct {
	t      *testing.T
	dir    string
	rec    *recorder
	cancel context.CancelFunc
	done   chan error
	w      *Watcher
}

func baseConfig(dir string, rec Notifier, accts ...Account) Config {
	return Config{
		Accounts:      accts,
		Filter:        policy.WatchSection{OnlyUnread: true},
		Notifier:      rec,
		Debounce:      150 * time.Millisecond,
		PollInterval:  20 * time.Millisecond,
		StateDir:      dir,
		Audit:         audit.New(dir),
		IdleMax:       time.Second,
		BackoffMin:    10 * time.Millisecond,
		BackoffMax:    40 * time.Millisecond,
		DegradedRetry: 30 * time.Millisecond,
	}
}

func start(t *testing.T, cfg Config) *harness {
	t.Helper()
	w, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, dir: cfg.StateDir, cancel: cancel, done: make(chan error, 1), w: w}
	if r, ok := cfg.Notifier.(*recorder); ok {
		h.rec = r
	}
	go func() { h.done <- w.Run(ctx) }()
	t.Cleanup(func() { h.stop() })
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run: %v", err)
		}
		h.done <- nil // let a second stop pass
	case <-time.After(5 * time.Second):
		h.t.Fatal("watch did not stop")
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitBaseline waits until the account has synced once (state has a cursor).
func waitBaseline(t *testing.T, dir, alias string) {
	eventually(t, "baseline", func() bool {
		st, err := loadState(statePath(dir, alias), "fake")
		return err == nil && st.Cursor != ""
	})
}

func TestDebounceMergesBurstIntoOneNotification(t *testing.T) {
	b := newBox()
	rec := &recorder{}
	h := start(t, baseConfig(t.TempDir(), rec, Account{Alias: "gmail", Provider: "fake", Open: b.open}))
	waitBaseline(t, h.dir, "gmail")

	b.deliver(msg("a", "alice@example.com", "one", true))
	time.Sleep(30 * time.Millisecond)
	b.deliver(msg("b", "bob@example.com", "two", true))
	time.Sleep(30 * time.Millisecond)
	b.deliver(msg("c", "carol@example.com", "three", true))

	eventually(t, "a notification", func() bool { return len(rec.payloads()) > 0 })
	time.Sleep(300 * time.Millisecond) // nothing more may follow
	got := rec.payloads()
	if len(got) != 1 || got[0].Count != 3 || got[0].Account != "gmail" || got[0].Source != "email-watch" {
		t.Fatalf("payloads: %+v", got)
	}
	var ids []string
	for _, m := range got[0].Messages {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Fatalf("ids %v", ids)
	}

	// The next burst is a new notification.
	b.deliver(msg("d", "dave@example.com", "four", true))
	eventually(t, "second notification", func() bool { return len(rec.payloads()) == 2 })
	if p := rec.payloads()[1]; p.Count != 1 || p.Messages[0].ID != "d" {
		t.Fatalf("second: %+v", p)
	}
}

func TestFiltersAndDuplicates(t *testing.T) {
	b := newBox()
	rec := &recorder{}
	cfg := baseConfig(t.TempDir(), rec, Account{Alias: "work", Provider: "fake", Open: b.open})
	cfg.Filter = policy.WatchSection{
		IgnoreSenders:  []string{"noreply@", "@mail.anthropic.com"},
		IgnoreSubjects: []string{"[newsletter]"},
		OnlyUnread:     true,
	}
	h := start(t, cfg)
	waitBaseline(t, h.dir, "work")

	b.deliver(
		msg("1", "NoReply@github.com", "build failed", true),
		msg("2", "news@mail.anthropic.com", "hello", true),
		msg("3", "x@example.com", "[Newsletter] October", true),
		msg("4", "x@example.com", "read on the phone", false),
		msg("5", "real@example.com", "keep me", true),
		msg("5", "real@example.com", "keep me", true), // the same id twice in one sync
	)
	eventually(t, "notification", func() bool { return len(rec.payloads()) == 1 })
	p := rec.payloads()[0]
	if p.Count != 1 || p.Messages[0].ID != "5" {
		t.Fatalf("payload %+v", p)
	}
	// The provider reports id 5 again (a delta change): not notified twice.
	b.deliver(msg("5", "real@example.com", "keep me", true))
	time.Sleep(300 * time.Millisecond)
	if n := len(rec.payloads()); n != 1 {
		t.Fatalf("notified %d times", n)
	}
}

func TestFilteredUnit(t *testing.T) {
	f := policy.WatchSection{IgnoreSenders: []string{"no-reply@"}, OnlyUnread: false}
	m := msg("1", "No-Reply@Example.com", "s", false)
	if filtered(f, &m) == "" {
		t.Fatal("sender filter is case-insensitive")
	}
	m = msg("1", "a@example.com", "s", false)
	if filtered(f, &m) != "" {
		t.Fatal("only_unread off: read mail passes")
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	b := newBox()
	rec := &recorder{}
	cfg := baseConfig(dir, rec, Account{Alias: "gmail", Provider: "fake", Open: b.open})
	cfg.Debounce = time.Hour // nothing is sent before the restart
	h := start(t, cfg)
	waitBaseline(t, dir, "gmail")
	b.deliver(msg("p1", "a@example.com", "queued", true))
	eventually(t, "queued", func() bool {
		st, _ := loadState(statePath(dir, "gmail"), "fake")
		return len(st.Pending) == 1
	})
	h.stop()
	if len(rec.payloads()) != 0 {
		t.Fatal("sent before the window ended")
	}

	// Restart with a short window: the queued message goes out once, and
	// the cursor kept from before means it is not queued twice.
	cfg.Debounce = 10 * time.Millisecond
	h2 := start(t, cfg)
	eventually(t, "notification after restart", func() bool { return len(rec.payloads()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := rec.payloads(); len(got) != 1 || got[0].Messages[0].ID != "p1" {
		t.Fatalf("after restart: %+v", got)
	}
	h2.stop()
	st, err := loadState(statePath(dir, "gmail"), "fake")
	if err != nil || len(st.Pending) != 0 || !slices.Contains(st.Notified, "p1") || st.Cursor != "1" {
		t.Fatalf("state %+v %v", st, err)
	}
	fi, _ := os.Stat(statePath(dir, "gmail"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v", fi.Mode())
	}
}

func TestRememberNotifiedKeepsNewest(t *testing.T) {
	st := &fileState{}
	for i := range 1005 {
		st.rememberNotified([]string{strconv.Itoa(i)})
	}
	if len(st.Notified) != maxNotified || st.Notified[0] != "5" || st.Notified[maxNotified-1] != "1004" {
		t.Fatalf("len %d first %s", len(st.Notified), st.Notified[0])
	}
}

func TestStateForOtherProviderDropsCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	_ = saveState(path, &fileState{Version: 1, Provider: "msgraph", Cursor: "x", Notified: []string{"1"}})
	st, err := loadState(path, "imap")
	if err != nil || st.Cursor != "" || st.Provider != "imap" || len(st.Notified) != 1 {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestPayloadShape(t *testing.T) {
	var items []Item
	for i := range 25 {
		m := msg(strconv.Itoa(i), "a@example.com", "s", true)
		items = append(items, itemOf(&m))
	}
	p := newPayload("work", items)
	if p.Count != 25 || len(p.Messages) != MaxPayloadMessages || p.Messages[0].ID != "0" {
		t.Fatalf("count %d len %d", p.Count, len(p.Messages))
	}
	b, _ := json.Marshal(newPayload("work", items[:1]))
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	keys := func(m map[string]any) []string {
		var k []string
		for x := range m {
			k = append(k, x)
		}
		slices.Sort(k)
		return k
	}
	if strings.Join(keys(got), ",") != "account,count,messages,source" {
		t.Fatalf("top keys %v", keys(got))
	}
	m0 := got["messages"].([]any)[0].(map[string]any)
	if strings.Join(keys(m0), ",") != "date,from,has_attachments,id,subject,thread_id,unread,web_link" {
		t.Fatalf("message keys %v", keys(m0))
	}
	if m0["date"] != "2026-10-09T15:22:49Z" || m0["from"].(map[string]any)["email"] != "a@example.com" {
		t.Fatalf("message %v", m0)
	}
	if b, _ := json.Marshal(newPayload("work", nil)); !strings.Contains(string(b), `"messages":[]`) {
		t.Fatalf("empty: %s", b)
	}
}

func TestHTTPNotifierRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		hdr = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	n := &HTTPNotifier{URL: srv.URL + "/webhooks/email", Token: "s3cret", Retries: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	m := msg("1", "a@example.com", "s", true)
	if err := n.Notify(context.Background(), newPayload("gmail", []Item{itemOf(&m)})); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || hdr.Get("X-Gitlab-Token") != "s3cret" || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("calls %d hdr %v", calls.Load(), hdr)
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil || p.Account != "gmail" || p.Count != 1 {
		t.Fatalf("%s %v", body, err)
	}
}

func TestHTTPNotifierGivesUpAfterThreeRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	n := &HTTPNotifier{URL: srv.URL, Retries: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	err := n.Notify(context.Background(), newPayload("gmail", nil))
	if err == nil || !strings.Contains(err.Error(), "401") || calls.Load() != 4 {
		t.Fatalf("err %v calls %d", err, calls.Load())
	}
}

func TestFailedNotificationIsDroppedAndAudited(t *testing.T) {
	dir := t.TempDir()
	b := newBox()
	rec := &recorder{fail: io.ErrUnexpectedEOF}
	h := start(t, baseConfig(dir, rec, Account{Alias: "gmail", Provider: "fake", Open: b.open}))
	waitBaseline(t, dir, "gmail")
	b.deliver(msg("x1", "a@example.com", "s", true), msg("x2", "b@example.com", "s", true))
	eventually(t, "attempt", func() bool { return len(rec.payloads()) == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(rec.payloads()); n != 1 {
		t.Fatalf("a dropped batch was sent again: %d", n)
	}
	h.stop()

	f, err := os.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []audit.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e audit.Entry
		_ = json.Unmarshal(sc.Bytes(), &e)
		entries = append(entries, e)
	}
	if len(entries) != 1 {
		t.Fatalf("audit %+v", entries)
	}
	e := entries[0]
	if e.Op != "watch.notify" || e.Account != "gmail" || e.Count != 2 || strings.Join(e.IDs, ",") != "x1,x2" ||
		e.Result != "error" || e.Error == "" {
		t.Fatalf("audit entry %+v", e)
	}
}

func TestDegradedAccountDoesNotStopOthers(t *testing.T) {
	good, bad := newBox(), newBox()
	bad.openErr = errs.Auth("token refresh failed (invalid_grant)")
	rec := &recorder{}
	cfg := baseConfig(t.TempDir(), rec,
		Account{Alias: "work", Provider: "fake", Open: bad.open},
		Account{Alias: "gmail", Provider: "fake", Open: good.open})
	h := start(t, cfg)
	waitBaseline(t, h.dir, "gmail")
	good.deliver(msg("g1", "a@example.com", "s", true))
	eventually(t, "gmail notification", func() bool { return len(rec.payloads()) == 1 })

	eventually(t, "retries of the degraded account", func() bool { return bad.opens.Load() >= 3 })
	code, body := h.w.Health()
	hb := body.(health)
	if code != http.StatusOK || hb.Status != "degraded" || !slices.Equal(hb.Degraded, []string{"work"}) {
		t.Fatalf("health %d %+v", code, hb)
	}
	for _, a := range hb.Accounts {
		if a.Account == "work" && (a.State != stateDegraded || !strings.Contains(a.Error, "invalid_grant")) {
			t.Fatalf("work: %+v", a)
		}
	}

	// A new login heals it without a restart.
	bad.mu.Lock()
	bad.openErr = nil
	bad.mu.Unlock()
	waitBaseline(t, h.dir, "work")
	eventually(t, "healthy", func() bool { _, b := h.w.Health(); return b.(health).Status == "ok" })
}

func TestHealthDownWhenAllOffline(t *testing.T) {
	bad := newBox()
	bad.openErr = errs.Provider("network", io.EOF)
	cfg := baseConfig(t.TempDir(), &recorder{}, Account{Alias: "gmail", Provider: "fake", Open: bad.open})
	cfg.OfflineLimit = 50 * time.Millisecond
	h := start(t, cfg)
	if code, _ := h.w.Health(); code != http.StatusOK {
		t.Fatalf("starting: %d", code)
	}
	eventually(t, "503", func() bool { code, _ := h.w.Health(); return code == http.StatusServiceUnavailable })
	srv := httptest.NewServer(h.w.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz %d", resp.StatusCode)
	}
	if bad.opens.Load() < 2 {
		t.Fatal("no reconnect attempts")
	}
}

func TestPollsWhenIdleIsUnsupported(t *testing.T) {
	b := newBox()
	b.idleErr = errs.Unsupported("no IDLE")
	rec := &recorder{}
	h := start(t, baseConfig(t.TempDir(), rec, Account{Alias: "gmail", Provider: "fake", Open: b.open}))
	waitBaseline(t, h.dir, "gmail")
	b.mu.Lock()
	b.msgs = append(b.msgs, msg("p", "a@example.com", "s", true)) // no wake-up: only a poll finds it
	b.mu.Unlock()
	eventually(t, "notification by polling", func() bool { return len(rec.payloads()) == 1 })
	_, body := h.w.Health()
	if w := body.(health).Accounts[0].Wait; w != waitPoll {
		t.Fatalf("wait %q", w)
	}
	if b.opens.Load() != 1 {
		t.Fatalf("unsupported IDLE must not reconnect: %d opens", b.opens.Load())
	}
}

func TestIdleErrorsReconnectThenFallBackToPolling(t *testing.T) {
	b := newBox()
	b.idleErr = errs.Provider("network", io.EOF)
	rec := &recorder{}
	cfg := baseConfig(t.TempDir(), rec, Account{Alias: "gmail", Provider: "fake", Open: b.open})
	cfg.IdleFailures = 2
	h := start(t, cfg)
	eventually(t, "fallback", func() bool {
		_, body := h.w.Health()
		a := body.(health).Accounts[0]
		return a.Wait == waitPoll && a.State == stateOnline
	})
	if b.opens.Load() < 2 || b.closes.Load() < 1 {
		t.Fatalf("opens %d closes %d", b.opens.Load(), b.closes.Load())
	}
	opens := b.opens.Load()
	time.Sleep(100 * time.Millisecond)
	if b.opens.Load() != opens {
		t.Fatal("still reconnecting while polling")
	}
}

func TestShutdownClosesConnection(t *testing.T) {
	b := newBox()
	h := start(t, baseConfig(t.TempDir(), &recorder{}, Account{Alias: "gmail", Provider: "fake", Open: b.open}))
	waitBaseline(t, h.dir, "gmail")
	h.stop()
	if b.closes.Load() != b.opens.Load() {
		t.Fatalf("opens %d closes %d", b.opens.Load(), b.closes.Load())
	}
}
