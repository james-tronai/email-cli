// Package watch is `email watch`: a long-running process that waits for
// new mail and wakes the agent with one HTTP notification per batch, so the
// agent spends no tokens while nothing arrives.
//
// Each account runs on its own: IMAP accounts wait with IDLE on INBOX
// (falling back to polling), the others poll their provider's sync cursor.
// New messages go through the [watch] policy filters into a per-account
// buffer; a batch is sent once the debounce window after its first message
// ends. Watch reads metadata only and never changes or sends mail.
package watch

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/james-tronai/email-cli/internal/audit"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

// Source is one account's mailbox, as watch uses it. Sync returns the mail
// that arrived after state ("" sets a baseline).
type Source interface {
	Sync(ctx context.Context, state string) (*provider.SyncResult, error)
	Close() error
}

// Idler is a Source that can wait for new mail on the server (IMAP IDLE).
// It returns errs.Unsupported when the server cannot.
type Idler interface {
	Idle(ctx context.Context, maxWait time.Duration) (bool, error)
}

type Account struct {
	Alias    string
	Provider string
	// Open connects. It runs again after every failure, so it should read
	// the account and its credentials afresh (a new login heals a
	// degraded account without a restart).
	Open func(ctx context.Context) (Source, error)
}

type Config struct {
	Accounts     []Account
	Filter       policy.WatchSection
	Notifier     Notifier
	Debounce     time.Duration
	PollInterval time.Duration
	StateDir     string
	HealthAddr   string // "" = no health endpoint
	Log          *slog.Logger
	Audit        *audit.Logger

	// Zero values get the defaults below; tests shorten them.
	IdleMax       time.Duration // renew IDLE this often (Gmail drops it at ~29 min)
	BackoffMin    time.Duration
	BackoffMax    time.Duration
	DegradedRetry time.Duration // how often a degraded account is retried and logged
	IdlePause     time.Duration // polling time after IdleFailures failed IDLEs
	IdleFailures  int
	OfflineLimit  time.Duration // /healthz is 503 once every account is offline this long
}

const (
	defaultIdleMax       = 20 * time.Minute
	defaultBackoffMin    = 5 * time.Second
	defaultBackoffMax    = 5 * time.Minute
	defaultDegradedRetry = 5 * time.Minute
	defaultIdlePause     = 30 * time.Minute
	defaultIdleFailures  = 3
	defaultOfflineLimit  = 10 * time.Minute
)

func (c *Config) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&c.IdleMax, defaultIdleMax)
	set(&c.BackoffMin, defaultBackoffMin)
	set(&c.BackoffMax, defaultBackoffMax)
	set(&c.DegradedRetry, defaultDegradedRetry)
	set(&c.IdlePause, defaultIdlePause)
	set(&c.OfflineLimit, defaultOfflineLimit)
	set(&c.PollInterval, 60*time.Second)
	if c.IdleFailures <= 0 {
		c.IdleFailures = defaultIdleFailures
	}
	if c.Debounce < 0 {
		c.Debounce = 0
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
}

// Account states, as /healthz shows them.
const (
	stateStarting  = "starting"
	stateOnline    = "online"
	stateOffline   = "reconnecting"
	stateDegraded  = "degraded" // needs a new login (or a policy change)
	waitIdle       = "idle"
	waitPoll       = "poll"
	eventNewMail   = "new_mail"
	eventNotifyOK  = "notify_ok"
	eventNotifyErr = "notify_failed"
)

type Watcher struct {
	cfg   Config
	accts []*account
	start time.Time
}

func New(cfg Config) (*Watcher, error) {
	cfg.defaults()
	if len(cfg.Accounts) == 0 {
		return nil, errs.Usage("watch: no accounts")
	}
	if cfg.Notifier == nil {
		return nil, errs.Usage("watch: no notifier")
	}
	if cfg.StateDir == "" {
		return nil, errs.Usage("watch: no state directory")
	}
	w := &Watcher{cfg: cfg, start: time.Now()}
	seen := map[string]bool{}
	for _, a := range cfg.Accounts {
		if seen[a.Alias] {
			return nil, errs.Usage("watch: account %s given twice", a.Alias)
		}
		seen[a.Alias] = true
		w.accts = append(w.accts, &account{w: w, cfg: a, kick: make(chan struct{}, 1), state: stateStarting, since: w.start,
			st: &fileState{}, notified: map[string]bool{}})
	}
	return w, nil
}

// Run watches until ctx ends, then closes the connections, saves state and
// returns nil. It fails only when it cannot start (state dir, health port).
func (w *Watcher) Run(ctx context.Context) error {
	for _, a := range w.accts {
		if err := a.load(); err != nil {
			return err
		}
	}
	var srv *http.Server
	if w.cfg.HealthAddr != "" {
		ln, err := net.Listen("tcp", w.cfg.HealthAddr)
		if err != nil {
			return errs.Usage("--health-addr %s: %v", w.cfg.HealthAddr, err)
		}
		srv = &http.Server{Handler: w.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	}
	aliases := make([]string, len(w.accts))
	for i, a := range w.accts {
		aliases[i] = a.cfg.Alias
	}
	w.cfg.Log.Info("watch started", "event", "start", "accounts", aliases,
		"debounce", w.cfg.Debounce.String(), "poll_interval", w.cfg.PollInterval.String())

	var wg sync.WaitGroup
	for _, a := range w.accts {
		wg.Add(2)
		go func() { defer wg.Done(); a.watchLoop(ctx) }()
		go func() { defer wg.Done(); a.batchLoop(ctx) }()
	}
	<-ctx.Done()
	wg.Wait()
	for _, a := range w.accts {
		a.mu.Lock()
		a.save()
		a.mu.Unlock()
	}
	if srv != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(sctx)
		cancel()
	}
	w.cfg.Log.Info("watch stopped", "event", "stop")
	return nil
}

// ---- health ----

type accountHealth struct {
	Account  string     `json:"account"`
	Provider string     `json:"provider"`
	State    string     `json:"state"`
	Wait     string     `json:"wait,omitempty"` // idle | poll
	LastSync *time.Time `json:"last_sync,omitempty"`
	Since    time.Time  `json:"since"` // when State began
	Error    string     `json:"error,omitempty"`
	Pending  int        `json:"pending"`
}

type health struct {
	Status   string          `json:"status"` // ok | degraded | down
	Accounts []accountHealth `json:"accounts"`
	Degraded []string        `json:"degraded"`
}

// Health is 200 while at least one account is online, and also while all
// are offline for less than OfflineLimit (starting up, a short outage);
// then 503. The body lists every account's state.
func (w *Watcher) Health() (int, any) {
	h := health{Status: "ok", Accounts: []accountHealth{}, Degraded: []string{}}
	online := false
	lastOnline := w.start
	for _, a := range w.accts {
		a.mu.Lock()
		ah := accountHealth{Account: a.cfg.Alias, Provider: a.cfg.Provider, State: a.state, Wait: a.wait,
			Since: a.since.UTC(), Error: a.lastErr, Pending: len(a.st.Pending)}
		if !a.lastSync.IsZero() {
			t := a.lastSync.UTC()
			ah.LastSync = &t
			if t.After(lastOnline) {
				lastOnline = t
			}
		}
		if a.state == stateOnline {
			online = true
		} else {
			h.Degraded = append(h.Degraded, a.cfg.Alias)
		}
		a.mu.Unlock()
		h.Accounts = append(h.Accounts, ah)
	}
	if len(h.Degraded) > 0 {
		h.Status = "degraded"
	}
	if !online && time.Since(lastOnline) > w.cfg.OfflineLimit {
		h.Status = "down"
		return http.StatusServiceUnavailable, h
	}
	return http.StatusOK, h
}

func (w *Watcher) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		code, body := w.Health()
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(code)
		_ = json.NewEncoder(rw).Encode(body)
	})
	return mux
}

// ---- one account ----

type account struct {
	w    *Watcher
	cfg  Account
	kick chan struct{}

	mu       sync.Mutex
	st       *fileState
	notified map[string]bool
	state    string
	wait     string
	since    time.Time
	lastSync time.Time
	lastErr  string
}

func (a *account) log() *slog.Logger { return a.w.cfg.Log.With("account", a.cfg.Alias) }

func (a *account) path() string { return statePath(a.w.cfg.StateDir, a.cfg.Alias) }

func (a *account) load() error {
	st, err := loadState(a.path(), a.cfg.Provider)
	if err != nil {
		a.log().Warn("state file unreadable; starting from a new baseline", "event", "state_reset", "error", err.Error())
	}
	if err := saveState(a.path(), st); err != nil {
		return errs.Usage("--state-dir: %v", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.st = st
	a.notified = map[string]bool{}
	for _, id := range st.Notified {
		a.notified[id] = true
	}
	return nil
}

// save writes the state; a.mu must be held. A failed write is logged: the
// worst outcome is a repeated or a missed notification after a restart.
func (a *account) save() {
	if err := saveState(a.path(), a.st); err != nil {
		a.log().Error("cannot save watch state", "event", "state_error", "error", err.Error())
	}
}

func (a *account) setState(state, errMsg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != state {
		a.state, a.since = state, time.Now()
	}
	a.lastErr = errMsg
}

func (a *account) setWait(wait string) {
	a.mu.Lock()
	a.wait = wait
	a.mu.Unlock()
}

// ingest queues the new messages that pass the filters, then moves the
// cursor; both are saved together so a crash can neither lose nor repeat a
// queued message.
func (a *account) ingest(res *provider.SyncResult) {
	log := a.log()
	now := time.Now()
	a.mu.Lock()
	queued := 0
	for i := range res.Messages {
		m := &res.Messages[i]
		if a.notified[m.ID] || slices.ContainsFunc(a.st.Pending, func(p pendingItem) bool { return p.ID == m.ID }) {
			continue
		}
		if why := filtered(a.w.cfg.Filter, m); why != "" {
			log.Debug("filtered", "event", "filtered", "id", m.ID, "from", m.From.Email, "subject", m.Subject, "reason", why)
			continue
		}
		a.st.Pending = append(a.st.Pending, pendingItem{Item: itemOf(m), QueuedAt: now})
		queued++
		log.Info("new mail", "event", eventNewMail, "id", m.ID, "from", m.From.Email, "subject", m.Subject)
	}
	a.st.Cursor = res.State
	a.lastSync = now
	a.save()
	a.mu.Unlock()
	if res.Initialized {
		log.Info("baseline set; mail from now on is reported", "event", "baseline")
	}
	if queued > 0 {
		select {
		case a.kick <- struct{}{}:
		default:
		}
	}
}

// watchLoop keeps a connection, syncs, and waits for more mail.
func (a *account) watchLoop(ctx context.Context) {
	cfg := &a.w.cfg
	log := a.log()
	backoff := time.Duration(0)
	failing := false // the last attempt failed
	idleFails := 0   // consecutive failed IDLEs
	var noIdleUntil time.Time
	for ctx.Err() == nil {
		err := a.session(ctx, &idleFails, &noIdleUntil, func() {
			if failing {
				log.Info("connected again", "event", "reconnected")
				failing = false
			}
			backoff = 0
		})
		if ctx.Err() != nil {
			return
		}
		failing = true
		var wait time.Duration
		switch code := errs.ExitCode(err); code {
		case errs.ExitAuth, errs.ExitPolicy:
			// Needs a person: a new login, or a policy that allows reading.
			a.setState(stateDegraded, err.Error())
			log.Warn("account needs attention; retrying every "+cfg.DegradedRetry.String(), "event", "degraded", "error", err.Error())
			wait = cfg.DegradedRetry
		default:
			if backoff == 0 {
				backoff = cfg.BackoffMin
			} else {
				backoff = min(backoff*2, cfg.BackoffMax)
			}
			a.setState(stateOffline, err.Error())
			log.Error("connection failed; retrying in "+backoff.String(), "event", "conn_error", "error", err.Error())
			wait = backoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// session runs one connection until it fails or ctx ends.
func (a *account) session(ctx context.Context, idleFails *int, noIdleUntil *time.Time, onSync func()) error {
	cfg := &a.w.cfg
	log := a.log()
	src, err := a.cfg.Open(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	idler, canIdle := src.(Idler)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.mu.Lock()
		cursor := a.st.Cursor
		a.mu.Unlock()
		res, err := src.Sync(ctx, cursor)
		if err != nil {
			if ctx.Err() == nil && cursor != "" && errs.From(err).Code == "usage" {
				// Not a cursor this provider understands: start over.
				log.Warn("sync cursor rejected; setting a new baseline", "event", "state_reset", "error", err.Error())
				a.mu.Lock()
				a.st.Cursor = ""
				a.save()
				a.mu.Unlock()
				continue
			}
			return err
		}
		a.ingest(res)
		a.setState(stateOnline, "")
		onSync()

		if canIdle && time.Now().After(*noIdleUntil) {
			a.setWait(waitIdle)
			_, err := idler.Idle(ctx, cfg.IdleMax)
			if err == nil {
				*idleFails = 0
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errs.From(err).Code == "unsupported" {
				log.Info("server has no IDLE; polling every "+cfg.PollInterval.String(), "event", "poll_fallback")
				canIdle = false
			} else {
				*idleFails++
				if *idleFails >= cfg.IdleFailures {
					*idleFails = 0
					*noIdleUntil = time.Now().Add(cfg.IdlePause)
					log.Warn("IDLE keeps failing; polling for "+cfg.IdlePause.String(), "event", "poll_fallback", "error", err.Error())
				}
				return err // the connection is gone
			}
		}
		a.setWait(waitPoll)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}

// batchLoop sends the queued messages once the debounce window after the
// oldest of them has passed.
func (a *account) batchLoop(ctx context.Context) {
	var timer *time.Timer
	var fire <-chan time.Time
	for {
		if fire == nil {
			a.mu.Lock()
			var oldest time.Time
			for _, p := range a.st.Pending {
				if oldest.IsZero() || p.QueuedAt.Before(oldest) {
					oldest = p.QueuedAt
				}
			}
			a.mu.Unlock()
			if !oldest.IsZero() {
				timer = time.NewTimer(max(0, time.Until(oldest.Add(a.w.cfg.Debounce))))
				fire = timer.C
			}
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-a.kick:
		case <-fire:
			fire = nil
			a.flush(ctx)
		}
	}
}

// flush notifies about everything queued. Messages queued while the
// notification is in flight wait for the next one.
func (a *account) flush(ctx context.Context) {
	a.mu.Lock()
	items := make([]Item, len(a.st.Pending))
	for i, p := range a.st.Pending {
		items[i] = p.Item
	}
	a.mu.Unlock()
	if len(items) == 0 {
		return
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	err := a.w.cfg.Notifier.Notify(ctx, newPayload(a.cfg.Alias, items))
	if err != nil && ctx.Err() != nil {
		// Shutting down: the batch stays queued in the state file and goes
		// out after the restart.
		return
	}
	a.mu.Lock()
	a.st.Pending = slices.DeleteFunc(a.st.Pending, func(p pendingItem) bool { return slices.Contains(ids, p.ID) })
	// A lost batch is not retried: the agent's fallback cron catches it.
	a.st.rememberNotified(ids)
	for _, id := range ids {
		a.notified[id] = true
	}
	if len(a.notified) > maxNotified*2 {
		a.notified = map[string]bool{}
		for _, id := range a.st.Notified {
			a.notified[id] = true
		}
	}
	a.save()
	a.mu.Unlock()

	e := audit.Entry{Account: a.cfg.Alias, Provider: a.cfg.Provider, Op: "watch.notify", Decision: "allow",
		Result: "ok", Count: len(ids), IDs: ids}
	if err != nil {
		e.Result, e.Error = "error", err.Error()
		a.log().Error("notification failed; batch dropped", "event", eventNotifyErr, "count", len(ids), "error", err.Error())
	} else {
		a.log().Info("notified", "event", eventNotifyOK, "count", len(ids))
	}
	if aerr := a.w.cfg.Audit.Log(e); aerr != nil {
		a.log().Error("audit log", "event", "audit_error", "error", aerr.Error())
	}
}
