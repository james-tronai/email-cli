package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/james-tronai/email-cli/internal/audit"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/provider/imap"
	"github.com/james-tronai/email-cli/internal/service"
	"github.com/james-tronai/email-cli/internal/watch"
)

type watchFlags struct {
	accounts     []string
	notifyURL    string
	notifyEnv    string
	debounce     time.Duration
	pollInterval time.Duration
	stateDir     string
	healthAddr   string
	logLevel     string
	noIdle       bool
}

func (a *app) watchCmd() *cobra.Command {
	var f watchFlags
	c := &cobra.Command{
		Use:   "watch [-a alias]... --notify-url URL",
		Short: "Wait for new mail and POST one notification per batch (long-running)",
		Long: `Runs until SIGTERM/SIGINT. Each account (-a, repeatable; default: all) waits
for new INBOX mail: IMAP IDLE on imap accounts, a sync poll every
--poll-interval on the others. New messages that pass the [watch] policy
filters are collected for --debounce, then POSTed as one JSON notification
per account. Only metadata is sent (no bodies); nothing is changed or sent
in the mailbox, and read mode is enough.

The token for the X-Gitlab-Token header is read from the environment
variable named by --notify-token-env, never from the command line.
With --dry-run, notifications are printed to stdout instead of POSTed.

Logs are JSON lines on stderr. GET /healthz on --health-addr is 200 while
at least one account is online, 503 once all have been offline 10 minutes.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runWatch(cmd.Context(), f) },
	}
	fl := c.Flags()
	// Shadows the global --account: watch takes several.
	fl.StringSliceVarP(&f.accounts, "account", "a", nil, "account alias to watch (repeatable; default: all accounts)")
	fl.StringVar(&f.notifyURL, "notify-url", "", "webhook URL to POST notifications to")
	fl.StringVar(&f.notifyEnv, "notify-token-env", "", "environment variable holding the webhook token (sent as X-Gitlab-Token)")
	fl.DurationVar(&f.debounce, "debounce", 60*time.Second, "collect new mail this long before notifying")
	fl.DurationVar(&f.pollInterval, "poll-interval", 60*time.Second, "poll interval (msgraph, gmail, and imap without IDLE)")
	fl.StringVar(&f.stateDir, "state-dir", "", "watch state (default: <state dir>/watch); separate from `email sync`")
	fl.StringVar(&f.healthAddr, "health-addr", "", "serve GET /healthz here, e.g. 127.0.0.1:8645 (loopback only)")
	fl.StringVar(&f.logLevel, "log-level", "info", "debug|info|warn|error (debug also logs filtered mail)")
	fl.BoolVar(&f.noIdle, "no-idle", false, "poll imap accounts too (for servers that accept IDLE but do not push; see `email doctor --idle-test`)")
	c.AddCommand(a.watchHealthCmd())
	return c
}

// watchHealthCmd is for an exec probe: the health endpoint listens on
// loopback only, which an httpGet probe from the kubelet cannot reach.
func (a *app) watchHealthCmd() *cobra.Command {
	var addr string
	c := &cobra.Command{
		Use:   "healthz --health-addr 127.0.0.1:8645",
		Short: "Query a running watch's /healthz; exit 0 on 200 (for an exec probe)",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkLoopback(addr); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return errs.Provider("unreachable", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			if !a.quiet {
				a.stdout.Write(body)
			}
			if resp.StatusCode != http.StatusOK {
				return errs.Provider("unhealthy", fmt.Errorf("watch is unhealthy: HTTP %d", resp.StatusCode))
			}
			return nil
		},
	}
	c.Flags().StringVar(&addr, "health-addr", "127.0.0.1:8645", "the watch's --health-addr")
	return c
}

func (a *app) runWatch(ctx context.Context, f watchFlags) error {
	log, err := newJSONLogger(a.stderr, f.logLevel)
	if err != nil {
		return err
	}
	var notifier watch.Notifier
	switch {
	case a.dryRun:
		notifier = &watch.WriterNotifier{W: a.stdout}
	case f.notifyURL == "":
		return errs.Usage("--notify-url is required (or --dry-run to print notifications)")
	default:
		u, err := url.Parse(f.notifyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errs.Usage("--notify-url must be an http(s) URL")
		}
		token := ""
		if f.notifyEnv != "" {
			if token = os.Getenv(f.notifyEnv); token == "" {
				return errs.Usage("--notify-token-env: $%s is empty or unset", f.notifyEnv)
			}
		}
		notifier = &watch.HTTPNotifier{URL: f.notifyURL, Token: token, Log: log}
	}
	if f.healthAddr != "" {
		if err := checkLoopback(f.healthAddr); err != nil {
			return err
		}
	}
	if f.pollInterval < 5*time.Second {
		return errs.Usage("--poll-interval must be at least 5s")
	}
	if f.debounce < 0 {
		return errs.Usage("--debounce must not be negative")
	}
	pol, err := a.loadPolicy()
	if err != nil {
		return err
	}

	aliases := f.accounts
	if len(aliases) == 0 {
		all, err := a.store.ListAccounts()
		if err != nil {
			return err
		}
		for _, ac := range all {
			aliases = append(aliases, ac.Alias)
		}
		if len(aliases) == 0 {
			return errs.Auth("no accounts configured; run `email auth login`")
		}
	}
	var accts []watch.Account
	for _, alias := range aliases {
		acct, err := a.store.LoadAccount(alias)
		if err != nil {
			return err
		}
		accts = append(accts, watch.Account{Alias: alias, Provider: acct.Provider, Open: a.watchSource(alias)})
	}

	stateDir := f.stateDir
	if stateDir == "" {
		stateDir = filepath.Join(a.stateDir, "watch")
	}
	w, err := watch.New(watch.Config{
		Accounts:     accts,
		Filter:       pol.Watch,
		Notifier:     notifier,
		Debounce:     f.debounce,
		PollInterval: f.pollInterval,
		StateDir:     stateDir,
		HealthAddr:   f.healthAddr,
		Log:          log,
		Audit:        audit.New(a.stateDir),
		NoIdle:       f.noIdle,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return w.Run(ctx)
}

// watchSource opens an account for watch. It reads the account, policy and
// credentials again on every call, so a new login takes effect on the next
// reconnect. The provider is opened in read mode at most: watch never
// changes mail, whatever the account may do.
func (a *app) watchSource(alias string) func(context.Context) (watch.Source, error) {
	return func(ctx context.Context) (watch.Source, error) {
		acct, err := a.store.LoadAccount(alias)
		if err != nil {
			return nil, err
		}
		pol, err := a.loadPolicy()
		if err != nil {
			return nil, err
		}
		p, err := a.openProvider(ctx, acct, min(pol.EffectiveMode(acct.Mode), policy.ModeRead))
		if err != nil {
			return nil, err
		}
		src := &serviceSource{
			svc: &service.Service{Account: acct, Provider: p, Policy: pol, Audit: audit.New(a.stateDir), StateDir: a.stateDir},
			p:   p,
		}
		if ip, ok := p.(*imap.Provider); ok {
			return &imapSource{serviceSource: src, imap: ip}, nil
		}
		return src, nil
	}
}

type serviceSource struct {
	svc *service.Service
	p   provider.Provider
}

func (s *serviceSource) Sync(ctx context.Context, state string) (*provider.SyncResult, error) {
	return s.svc.SyncFrom(ctx, provider.LabelInbox, state)
}

func (s *serviceSource) Close() error {
	if c, ok := s.p.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type imapSource struct {
	*serviceSource
	imap *imap.Provider
}

func (s *imapSource) Idle(ctx context.Context, maxWait time.Duration) (bool, error) {
	return s.imap.Idle(ctx, maxWait)
}

// newJSONLogger logs one JSON object per line: ts, level, msg, then the
// attributes (account, event, …).
func newJSONLogger(w io.Writer, level string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, errs.Usage("--log-level: %v", err)
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv, ReplaceAttr: func(groups []string, at slog.Attr) slog.Attr {
		if len(groups) > 0 {
			return at
		}
		switch at.Key {
		case slog.TimeKey:
			return slog.String("ts", at.Value.Time().UTC().Format(time.RFC3339Nano))
		case slog.LevelKey:
			return slog.String("level", strings.ToLower(at.Value.String()))
		}
		return at
	}})
	return slog.New(h), nil
}

// checkLoopback refuses a health address that is reachable from outside the
// Pod.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errs.Usage("--health-addr %q: %v", addr, err)
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return errs.Usage("--health-addr must be a loopback address (127.0.0.1:<port>), not %q", addr)
	}
	return nil
}
