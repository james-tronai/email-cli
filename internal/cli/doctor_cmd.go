package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
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

// Check results.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

type check struct {
	Name    string `json:"name"`
	Account string `json:"account,omitempty"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix,omitempty"`
}

type doctorReport struct {
	OK     bool    `json:"ok"`
	Checks []check `json:"checks"`
}

func (r *doctorReport) add(c check) { r.Checks = append(r.Checks, c) }

func (r doctorReport) WriteText(w io.Writer) error {
	marks := map[string]string{checkOK: "ok  ", checkWarn: "WARN", checkFail: "FAIL", checkSkip: "--  "}
	for _, c := range r.Checks {
		name := c.Name
		if c.Account != "" {
			name = c.Account + ": " + name
		}
		fmt.Fprintf(w, "[%s] %s: %s\n", marks[c.Status], name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "       fix: %s\n", c.Fix)
		}
	}
	if r.OK {
		fmt.Fprintln(w, "\nall checks passed")
	} else {
		fmt.Fprintln(w, "\nsome checks failed")
	}
	return nil
}

type doctorFlags struct {
	accounts  []string
	idleTest  time.Duration
	notifyURL string
	stateDir  string
	noUpdate  bool
}

func (a *app) doctorCmd() *cobra.Command {
	var f doctorFlags
	c := &cobra.Command{
		Use:   "doctor [-a alias]...",
		Short: "Check the installation, policy, accounts and watch setup (read-only)",
		Long: `Runs read-only checks and says how to fix what fails:

  binary     version, and whether a newer release exists
  policy     the policy file parses; the mode it sets
  accounts   each account logs in and can list one message (nothing is
             printed from it); imap: what the server supports
  watch      the watch state files; --notify-url: the webhook answers
             (a GET; nothing is posted)

--idle-test 60s checks that an imap server really pushes new mail during
IDLE: send yourself a message while it waits. Some servers accept IDLE but
never report anything; for those, run watch with --no-idle.

Exit 0 when nothing failed (warnings allowed), 1 otherwise.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := a.runDoctor(cmd.Context(), f)
			if err := a.output(rep); err != nil {
				return err
			}
			if !rep.OK {
				return &errs.Error{Exit: errs.ExitInternal, Code: "doctor_failed", Message: "doctor: some checks failed"}
			}
			return nil
		},
	}
	fl := c.Flags()
	fl.StringSliceVarP(&f.accounts, "account", "a", nil, "account to check (repeatable; default: all)")
	fl.DurationVar(&f.idleTest, "idle-test", 0, "imap: wait this long in IDLE for a test message you send (e.g. 60s)")
	fl.StringVar(&f.notifyURL, "notify-url", "", "watch webhook to probe with a GET")
	fl.StringVar(&f.stateDir, "state-dir", "", "watch state directory (default: <state dir>/watch)")
	fl.BoolVar(&f.noUpdate, "no-update-check", false, "do not ask GitHub for the latest release")
	return c
}

func (a *app) runDoctor(ctx context.Context, f doctorFlags) doctorReport {
	rep := doctorReport{Checks: []check{}}

	// Binary.
	bin := check{Name: "binary", Status: checkOK, Detail: "email " + a.build.Version}
	if !f.noUpdate {
		vc := a.checkVersion(ctx)
		switch {
		case vc.CheckError != "":
			bin.Detail += " (update check failed: " + vc.CheckError + ")"
		case vc.UpdateAvailable:
			bin.Status, bin.Detail, bin.Fix = checkWarn, fmt.Sprintf("email %s; %s is available", a.build.Version, vc.Latest), vc.Install
		default:
			bin.Detail += " (latest)"
		}
	}
	rep.add(bin)

	// Policy.
	path := policy.DefaultPath(a.store.Dir)
	pol, err := policy.Load(path)
	switch {
	case err != nil:
		rep.add(check{Name: "policy", Status: checkFail, Detail: err.Error(),
			Fix: "fix the file; every command is refused until it parses (keys: docs/policy.example.toml)"})
	case pol.Source == "builtin":
		rep.add(check{Name: "policy", Status: checkWarn, Detail: "no policy file at " + path + ": built-in read-only policy",
			Fix: "copy docs/policy/draft.toml to " + path + " (or set $EMAIL_POLICY_FILE)"})
	default:
		rep.add(check{Name: "policy", Status: checkOK, Detail: fmt.Sprintf("%s: mode %s", pol.Source, pol.Default.Mode)})
	}

	// Accounts.
	aliases := f.accounts
	if len(aliases) == 0 {
		all, err := a.store.ListAccounts()
		if err != nil {
			rep.add(check{Name: "accounts", Status: checkFail, Detail: err.Error()})
		}
		for _, ac := range all {
			aliases = append(aliases, ac.Alias)
		}
		if err == nil && len(aliases) == 0 {
			rep.add(check{Name: "accounts", Status: checkFail, Detail: "no accounts in " + a.store.Dir,
				Fix: "email auth login -a <alias> --provider <msgraph|gmail|imap> … (see README)"})
		}
	}
	if pol == nil {
		pol = policy.Builtin()
	}
	for _, alias := range aliases {
		a.doctorAccount(ctx, &rep, pol, alias, f)
	}

	// Watch.
	stateDir := f.stateDir
	if stateDir == "" {
		stateDir = filepath.Join(a.stateDir, "watch")
	}
	for _, alias := range aliases {
		rep.add(watchStateCheck(stateDir, alias))
	}
	if f.notifyURL != "" {
		rep.add(probeWebhook(ctx, f.notifyURL))
	}

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == checkFail {
			rep.OK = false
		}
	}
	return rep
}

func (a *app) doctorAccount(ctx context.Context, rep *doctorReport, pol *policy.Policy, alias string, f doctorFlags) {
	acct, err := a.store.LoadAccount(alias)
	if err != nil {
		rep.add(check{Name: "login", Account: alias, Status: checkFail, Detail: errs.From(err).Message,
			Fix: "email auth login -a " + alias + " …"})
		return
	}
	mode := pol.EffectiveMode(acct.Mode)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second+f.idleTest)
	defer cancel()
	// Read mode at most: doctor changes nothing.
	p, err := a.openProvider(ctx, acct, min(mode, policy.ModeRead))
	if err == nil {
		if c, ok := p.(io.Closer); ok {
			defer c.Close()
		}
		svc := &service.Service{Account: acct, Provider: p, Policy: pol, Audit: audit.New(a.stateDir), StateDir: a.stateDir}
		var pg *provider.Page
		pg, err = svc.List(ctx, provider.ListOptions{Limit: 1})
		if err == nil {
			detail := fmt.Sprintf("%s %s, login mode %s, effective mode %s; inbox readable (%d message listed)",
				acct.Provider, acct.Username, acct.Mode, mode, len(pg.Messages))
			rep.add(check{Name: "login", Account: alias, Status: checkOK, Detail: detail})
		}
	}
	if err != nil {
		e := errs.From(err)
		fix := "see the message; then run email doctor again"
		switch e.Exit {
		case errs.ExitAuth:
			fix = "log in again: email auth login -a " + alias + " --provider " + acct.Provider + " --mode " + acct.Mode.String() + " …"
		case errs.ExitPolicy:
			fix = "the policy does not allow reading: set [default] mode to read or higher"
		case errs.ExitTimeout:
			fix = "check the network path to the mail server (egress, DNS, firewall)"
		}
		rep.add(check{Name: "login", Account: alias, Status: checkFail, Detail: e.Message, Fix: fix})
		return
	}
	if ip, ok := p.(*imap.Provider); ok {
		a.doctorIMAP(ctx, rep, ip, alias, acct.Username, f.idleTest)
	}
}

func (a *app) doctorIMAP(ctx context.Context, rep *doctorReport, p *imap.Provider, alias, addr string, wait time.Duration) {
	info, err := p.ServerInfo(ctx)
	if err != nil {
		rep.add(check{Name: "imap server", Account: alias, Status: checkFail, Detail: errs.From(err).Message})
		return
	}
	d := fmt.Sprintf("IDLE %v, CONDSTORE %v, Gmail extensions %v", info.Idle, info.CondStore, info.Gmail)
	st := check{Name: "imap server", Account: alias, Status: checkOK, Detail: d}
	if !info.Idle {
		st.Status, st.Detail = checkWarn, d+"; watch will poll every --poll-interval"
	}
	rep.add(st)
	if !info.Idle {
		return
	}
	if wait <= 0 {
		rep.add(check{Name: "idle push", Account: alias, Status: checkSkip,
			Detail: "not tested; run `email doctor -a " + alias + " --idle-test 60s` and send yourself a message while it waits"})
		return
	}
	before, err := p.InboxUIDNext(ctx)
	if err != nil {
		rep.add(check{Name: "idle push", Account: alias, Status: checkFail, Detail: errs.From(err).Message})
		return
	}
	fmt.Fprintf(a.stderr, "doctor: waiting up to %s in IDLE on %s's INBOX: send a message to %s now\n", wait, alias, addr)
	woke, err := p.Idle(ctx, wait)
	if err != nil {
		rep.add(check{Name: "idle push", Account: alias, Status: checkFail, Detail: "IDLE failed: " + errs.From(err).Message,
			Fix: "run watch with --no-idle"})
		return
	}
	if woke {
		rep.add(check{Name: "idle push", Account: alias, Status: checkOK, Detail: "the server reported the new message during IDLE"})
		return
	}
	// Did mail arrive that IDLE did not report?
	after, err := p.InboxUIDNext(ctx)
	switch {
	case err != nil:
		rep.add(check{Name: "idle push", Account: alias, Status: checkWarn, Detail: "no push seen; could not recheck: " + errs.From(err).Message})
	case after > before:
		rep.add(check{Name: "idle push", Account: alias, Status: checkFail,
			Detail: "a message arrived but the server did not report it during IDLE",
			Fix:    "run watch with --no-idle (polls every --poll-interval)"})
	default:
		rep.add(check{Name: "idle push", Account: alias, Status: checkSkip, Detail: "no message arrived during the test; send one and run it again"})
	}
}

func watchStateCheck(dir, alias string) check {
	path := filepath.Join(dir, alias+".json")
	st, err := watch.ReadState(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return check{Name: "watch state", Account: alias, Status: checkSkip, Detail: "no " + path + " (watch has not run for this account)"}
	case err != nil:
		return check{Name: "watch state", Account: alias, Status: checkWarn, Detail: path + ": " + err.Error(),
			Fix: "watch starts from a new baseline when it cannot read the file"}
	}
	d := fmt.Sprintf("%s: provider %s, %d notified, %d waiting for the debounce window", path, st.Provider, len(st.Notified), st.Pending)
	if st.Cursor == "" {
		d += ", no cursor yet"
	}
	return check{Name: "watch state", Account: alias, Status: checkOK, Detail: d}
}

// probeWebhook only GETs the URL: a POST would wake the agent.
func probeWebhook(ctx context.Context, raw string) check {
	c := check{Name: "webhook"}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		c.Status, c.Detail = checkFail, "--notify-url must be an http(s) URL"
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.Status, c.Detail, c.Fix = checkFail, "unreachable: "+err.Error(), "is the agent's webhook listening, and does the network allow watch to reach it?"
		return c
	}
	resp.Body.Close()
	c.Status, c.Detail = checkOK, fmt.Sprintf("%s answered HTTP %d to a GET (reachable; the token is only checked on a real notification)", u.Host, resp.StatusCode)
	if resp.StatusCode >= 500 {
		c.Status = checkWarn
	}
	return c
}
