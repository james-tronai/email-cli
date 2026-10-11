package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	emailcli "github.com/james-tronai/email-cli"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/hermes"
	"github.com/james-tronai/email-cli/internal/policy"
)

func (a *app) hermesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hermes",
		Short: "Connect email to a Hermes agent (skill, webhook route, triage cron, watch service)",
		Long: `Sets up a Hermes agent to use email (docs/hermes.md):

  install    single host: everything below plus a watch service (systemd or launchd)
  files      write the skill and the webhook route script into HERMES_HOME
  seed-cron  create or update the email-triage cron job (via the hermes CLI)
  config     print the config.yaml snippet (webhook route, skills) to merge by hand
  skill      print the skill (SKILL.md)

The skill, script and cron job are the ones released with this binary.`,
	}
	c.AddCommand(a.hermesInstallCmd(), a.hermesFilesCmd(), a.hermesSeedCronCmd(), a.hermesConfigCmd(), a.hermesSkillCmd())
	return c
}

func hermesHome(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	if h := os.Getenv("HERMES_HOME"); h != "" {
		return filepath.Abs(h)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errs.Usage("--hermes-home is required ($HERMES_HOME and $HOME are unset)")
	}
	return filepath.Join(home, ".hermes"), nil
}

// filesFlags are shared by files and install.
type filesFlags struct {
	notesFile string
	timezone  string
	labels    []string
}

func (f *filesFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&f.notesFile, "skill-notes", "", "file appended to the skill as \"## This deployment\" (accounts, house rules, language)")
	c.Flags().StringVar(&f.timezone, "timezone", "", "IANA time zone for times in new-mail notifications (default: the system's)")
	c.Flags().StringArrayVar(&f.labels, "account-label", nil, "alias=label shown in new-mail notifications (repeatable)")
}

func (f *filesFlags) options() (hermes.FilesOptions, error) {
	var o hermes.FilesOptions
	if f.notesFile != "" {
		b, err := os.ReadFile(f.notesFile)
		if err != nil {
			return o, errs.Usage("--skill-notes: %v", err)
		}
		o.Notes = string(b)
	}
	labels, err := hermes.ParseLabels(f.labels)
	if err != nil {
		return o, errs.Usage("%v", err)
	}
	o.Timezone, o.Labels = f.timezone, labels
	if _, err := hermes.RouteSettings(o); err != nil {
		return o, errs.Usage("--timezone: %v", err)
	}
	return o, nil
}

func (a *app) hermesFilesCmd() *cobra.Command {
	var home string
	var ff filesFlags
	c := &cobra.Command{
		Use:   "files [--hermes-home DIR]",
		Short: "Write the skill and the webhook route script into HERMES_HOME",
		Long: `Writes, replacing local edits:
  skills/comms/email/SKILL.md   the email skill (plus --skill-notes)
  scripts/email-summary.py      the webhook route script for email watch
  scripts/email-summary.json    --timezone and --account-label (removed when neither is given)

HERMES_HOME is --hermes-home, else $HERMES_HOME, else ~/.hermes. Safe to run
on every start (an initContainer does): files are only rewritten when their
content changes.`,
		Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			h, err := hermesHome(home)
			if err != nil {
				return err
			}
			o, err := ff.options()
			if err != nil {
				return err
			}
			if a.dryRun {
				return a.output(map[string]any{"hermes_home": h, "dry_run": true})
			}
			res, err := hermes.WriteFiles(h, o)
			if err != nil {
				return errs.Usage("%v", err)
			}
			return a.output(map[string]any{"hermes_home": h, "files": res})
		},
	}
	c.Flags().StringVar(&home, "hermes-home", "", "Hermes home directory (default $HERMES_HOME, else ~/.hermes)")
	ff.register(c)
	return c
}

// cronFlags are shared by seed-cron and install.
type cronFlags struct {
	deliver    string
	hermesBin  string
	name       string
	schedule   string
	promptFile string
}

func (f *cronFlags) register(c *cobra.Command, deliverHelp string) {
	c.Flags().StringVar(&f.deliver, "deliver", "", deliverHelp)
	c.Flags().StringVar(&f.hermesBin, "hermes-bin", "hermes", "the hermes CLI")
	c.Flags().StringVar(&f.name, "cron-name", "", "job name (default: email-triage; the webhook route's cron_job must match)")
	c.Flags().StringVar(&f.schedule, "schedule", "", "fallback schedule (default: every 2h)")
	c.Flags().StringVar(&f.promptFile, "prompt-file", "", "use this prompt instead of the released one")
}

func (f *cronFlags) job() (hermes.CronJob, error) {
	j, err := hermes.DefaultCronJob()
	if err != nil {
		return j, err
	}
	if f.name != "" {
		j.Name = f.name
	}
	if f.schedule != "" {
		j.Schedule = f.schedule
	}
	if f.promptFile != "" {
		b, err := os.ReadFile(f.promptFile)
		if err != nil {
			return j, errs.Usage("--prompt-file: %v", err)
		}
		j.Prompt = strings.TrimSpace(string(b))
	}
	return j, nil
}

func (a *app) hermesSeedCronCmd() *cobra.Command {
	var home string
	var cf cronFlags
	c := &cobra.Command{
		Use:   "seed-cron --deliver PLATFORM:CHAT_ID [--hermes-home DIR]",
		Short: "Create or update the email-triage cron job (idempotent)",
		Long: `Creates the email-triage cron job with ` + "`hermes cron create`" + `, or, if a job of
that name exists, brings it in line with ` + "`hermes cron edit`" + `. A job that already
matches is not touched, so this is safe to run on every start. A job you
paused stays paused.

The job runs every 2h as a fallback; the webhook route (` + "`email hermes config`" + `)
runs it at once when email watch reports new mail. Hermes's cron jobs live in
HERMES_HOME/cron/jobs.json, outside Git; this makes them reproducible.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := hermesHome(home)
			if err != nil {
				return err
			}
			j, err := cf.job()
			if err != nil {
				return err
			}
			res, err := hermes.SeedCron(cmd.Context(), hermes.SeedOptions{
				Home: h, HermesBin: cf.hermesBin, Deliver: cf.deliver, Job: j, DryRun: a.dryRun,
			})
			if err != nil {
				return errs.Usage("%v", err)
			}
			return a.output(res)
		},
	}
	c.Flags().StringVar(&home, "hermes-home", "", "Hermes home directory (default $HERMES_HOME, else ~/.hermes)")
	cf.register(c, "where reports go: platform:chat_id (discord:123…), origin, local, …")
	return c
}

func (a *app) hermesConfigCmd() *cobra.Command {
	var o hermes.ConfigOptions
	c := &cobra.Command{
		Use:   "config",
		Short: "Print the Hermes config.yaml snippet (webhook route and skills) to merge by hand",
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			if o.CronJob == "" {
				j, err := hermes.DefaultCronJob()
				if err != nil {
					return err
				}
				o.CronJob = j.Name
			}
			_, err := io.WriteString(a.stdout, hermes.ConfigSnippet(o))
			return err
		},
	}
	c.Flags().StringVar(&o.RouteName, "route", hermes.DefaultRouteName, "webhook route name (watch posts to /webhooks/<route>)")
	c.Flags().StringVar(&o.CronJob, "cron-job", "", "cron job the route runs (default: email-triage)")
	c.Flags().IntVar(&o.Port, "port", hermes.DefaultWebhookPort, "Hermes webhook port")
	return c
}

func (a *app) hermesSkillCmd() *cobra.Command {
	var notes string
	c := &cobra.Command{
		Use:   "skill [--skill-notes FILE]",
		Short: "Print the email skill (SKILL.md), for Hermes or any agent that reads skills",
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			ff := filesFlags{notesFile: notes}
			o, err := ff.options()
			if err != nil {
				return err
			}
			_, err = a.stdout.Write(hermes.Skill(o.Notes))
			return err
		},
	}
	c.Flags().StringVar(&notes, "skill-notes", "", "file appended as \"## This deployment\"")
	return c
}

// installReport is the result of `email hermes install`.
type installReport struct {
	HermesHome string              `json:"hermes_home"`
	Files      []hermes.FileResult `json:"files"`
	Policy     struct {
		Path     string `json:"path"`
		Template string `json:"template,omitempty"`
		Written  bool   `json:"written"`
	} `json:"policy"`
	HermesEnv struct {
		Path  string   `json:"path"`
		Added []string `json:"added,omitempty"`
	} `json:"hermes_env"`
	TokenFile string `json:"token_file"`
	Service   struct {
		Kind    string `json:"kind"` // systemd | launchd | none
		Path    string `json:"path,omitempty"`
		Changed bool   `json:"changed,omitempty"`
		Started bool   `json:"started"`
		Note    string `json:"note,omitempty"`
	} `json:"service"`
	Cron          *hermes.SeedResult `json:"cron,omitempty"`
	CronNote      string             `json:"cron_note,omitempty"`
	HermesConfig  string             `json:"hermes_config"`
	NextSteps     []string           `json:"next_steps"`
	AccountsFound []string           `json:"accounts"`
}

func (a *app) hermesInstallCmd() *cobra.Command {
	var (
		home, template, service, emailBin, notifyURL, healthAddr string
		accounts                                                 []string
		noStart                                                  bool
		ff                                                       filesFlags
		cf                                                       cronFlags
	)
	c := &cobra.Command{
		Use:   "install [--deliver PLATFORM:CHAT_ID] [-a alias]...",
		Short: "Set up Hermes on this machine: skill, route script, policy, watch service, triage cron",
		Long: `For Hermes running on this machine (not Kubernetes; see docs/hermes.md):

  1. writes the skill and the route script into HERMES_HOME (email hermes files)
  2. writes the policy from --policy-template, unless a policy file exists
  3. adds WEBHOOK_ENABLED and WEBHOOK_SECRET to HERMES_HOME/.env if missing,
     and puts the secret into <config dir>/watch.env (0600) for watch
  4. installs email watch as a systemd user service (Linux) or a launchd
     agent (macOS), and starts it once an account is logged in
  5. with --deliver, creates the email-triage cron job (email hermes seed-cron)
  6. prints the config.yaml snippet to merge into HERMES_HOME/config.yaml

Running it again updates everything in place. Log in first
(` + "`email auth login`" + `), then install; restart Hermes after merging the snippet.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.dryRun {
				return errs.Usage("install has no --dry-run; `email hermes config` and `email hermes seed-cron --dry-run` show what it would do")
			}
			h, err := hermesHome(home)
			if err != nil {
				return err
			}
			if st, err := os.Stat(h); err != nil || !st.IsDir() {
				return errs.Usage("no Hermes home at %s: install Hermes first, or pass --hermes-home", h)
			}
			fo, err := ff.options()
			if err != nil {
				return err
			}
			job, err := cf.job()
			if err != nil {
				return err
			}
			if _, err := emailcli.PolicyTemplates.ReadFile("docs/policy/" + template + ".toml"); err != nil {
				return errs.Usage("--policy-template %q: want read, draft, send-reply-only or send-open", template)
			}
			rep := &installReport{HermesHome: h}

			if rep.Files, err = hermes.WriteFiles(h, fo); err != nil {
				return errs.Usage("%v", err)
			}
			if err := a.installPolicy(rep, template); err != nil {
				return err
			}
			token, err := ensureWebhookSecret(rep, h)
			if err != nil {
				return err
			}
			rep.TokenFile = filepath.Join(a.store.Dir, "watch.env")
			if _, err := hermes.WriteIfChanged(rep.TokenFile, []byte(hermes.NotifyTokenEnv+"="+token+"\n"), 0o600); err != nil {
				return err
			}

			all, err := a.store.ListAccounts()
			if err != nil {
				return err
			}
			for _, ac := range all {
				rep.AccountsFound = append(rep.AccountsFound, ac.Alias)
			}
			if emailBin == "" {
				if emailBin, err = os.Executable(); err != nil {
					return err
				}
			}
			so := hermes.ServiceOptions{
				Bin: emailBin, Accounts: accounts, NotifyURL: notifyURL, TokenFile: rep.TokenFile,
				ConfigDir: a.store.Dir, StateDir: a.stateDir, PolicyFile: os.Getenv("EMAIL_POLICY_FILE"),
				HealthAddr: healthAddr,
			}
			if err := installService(cmd.Context(), rep, service, so, !noStart && len(all) > 0); err != nil {
				return err
			}
			if len(all) == 0 {
				rep.NextSteps = append(rep.NextSteps, "log in to a mailbox (`email auth login …`), then run `email hermes install` again to start watch")
			}

			if cf.deliver != "" {
				if rep.Cron, err = hermes.SeedCron(cmd.Context(), hermes.SeedOptions{
					Home: h, HermesBin: cf.hermesBin, Deliver: cf.deliver, Job: job,
				}); err != nil {
					return errs.Usage("%v", err)
				}
			} else {
				rep.CronNote = "no --deliver: the triage cron job was not created"
				rep.NextSteps = append(rep.NextSteps, "create the triage cron job: email hermes seed-cron --deliver <platform>:<chat_id>")
			}
			rep.HermesConfig = hermes.ConfigSnippet(hermes.ConfigOptions{CronJob: job.Name})
			rep.NextSteps = append(rep.NextSteps,
				"merge the config snippet into "+filepath.Join(h, "config.yaml")+", then restart Hermes",
				"check: email doctor --notify-url "+notifyURL+", then send yourself a mail; Hermes should message you within 60 to 90 seconds")
			if a.format == "text" || (!cmd.Flags().Changed("format") && a.jq == "" && isTerminal(a.stdout)) {
				return writeInstallText(a.stdout, rep)
			}
			return a.output(rep)
		},
	}
	c.Flags().StringVar(&home, "hermes-home", "", "Hermes home directory (default $HERMES_HOME, else ~/.hermes)")
	c.Flags().StringVar(&template, "policy-template", "draft", "policy written when none exists: read | draft | send-reply-only | send-open")
	c.Flags().StringVar(&service, "service", "auto", "auto | systemd | launchd | none")
	c.Flags().StringVar(&emailBin, "email-bin", "", "email binary the service runs (default: this one)")
	c.Flags().StringVar(&notifyURL, "notify-url", "http://127.0.0.1:8644/webhooks/email", "Hermes webhook URL for watch")
	c.Flags().StringVar(&healthAddr, "health-addr", "127.0.0.1:8645", "watch's /healthz address (loopback)")
	c.Flags().StringSliceVarP(&accounts, "account", "a", nil, "accounts to watch (repeatable; default: all)")
	c.Flags().BoolVar(&noStart, "no-start", false, "write the service but do not (re)start it")
	ff.register(c)
	cf.register(c, "create the triage cron job, reporting to platform:chat_id (discord:123…)")
	return c
}

func (a *app) installPolicy(rep *installReport, template string) error {
	path := policy.DefaultPath(a.store.Dir)
	rep.Policy.Path = path
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	data, err := emailcli.PolicyTemplates.ReadFile("docs/policy/" + template + ".toml")
	if err != nil {
		return errs.Usage("--policy-template %q: want read, draft, send-reply-only or send-open", template)
	}
	if _, err := hermes.WriteIfChanged(path, data, 0o644); err != nil {
		return err
	}
	rep.Policy.Template, rep.Policy.Written = template, true
	return nil
}

// ensureWebhookSecret returns the webhook token for watch: $EMAIL_WATCH_NOTIFY_TOKEN,
// else WEBHOOK_SECRET from HERMES_HOME/.env, which is created (with
// WEBHOOK_ENABLED=true) when missing.
func ensureWebhookSecret(rep *installReport, home string) (string, error) {
	envPath := filepath.Join(home, ".env")
	rep.HermesEnv.Path = envPath
	vars := map[string]string{}
	if f, err := os.Open(envPath); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if k, v, ok := strings.Cut(strings.TrimPrefix(l, "export "), "="); ok && !strings.HasPrefix(l, "#") {
				vars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var add []string
	if vars["WEBHOOK_ENABLED"] == "" {
		add = append(add, "WEBHOOK_ENABLED=true")
	}
	secret := vars["WEBHOOK_SECRET"]
	if secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		secret = hex.EncodeToString(b)
		add = append(add, "WEBHOOK_SECRET="+secret)
	}
	if len(add) > 0 {
		f, err := os.OpenFile(envPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		_, err = fmt.Fprintf(f, "\n# Added by `email hermes install`: the webhook that email watch posts to.\n%s\n", strings.Join(add, "\n"))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		for _, kv := range add {
			k, _, _ := strings.Cut(kv, "=")
			rep.HermesEnv.Added = append(rep.HermesEnv.Added, k)
		}
	}
	if t := os.Getenv(hermes.NotifyTokenEnv); t != "" {
		return t, nil
	}
	return secret, nil
}

func installService(ctx context.Context, rep *installReport, kind string, so hermes.ServiceOptions, start bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if kind == "auto" {
		switch {
		case runtime.GOOS == "darwin":
			kind = "launchd"
		case runtime.GOOS == "linux":
			if _, err := exec.LookPath("systemctl"); err == nil {
				kind = "systemd"
			} else {
				kind = "none"
			}
		default:
			kind = "none"
		}
	}
	rep.Service.Kind = kind
	run := func(name string, args ...string) error {
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	switch kind {
	case "systemd":
		rep.Service.Path = filepath.Join(home, ".config", "systemd", "user", hermes.SystemdUnitName)
		if rep.Service.Changed, err = hermes.WriteIfChanged(rep.Service.Path, []byte(hermes.SystemdUnit(so)), 0o644); err != nil {
			return err
		}
		if !start {
			rep.Service.Note = "not started"
			return nil
		}
		for _, args := range [][]string{
			{"--user", "daemon-reload"},
			{"--user", "enable", hermes.SystemdUnitName},
			{"--user", "restart", hermes.SystemdUnitName},
		} {
			if err := run("systemctl", args...); err != nil {
				rep.Service.Note = err.Error()
				return nil
			}
		}
		rep.Service.Started = true
		rep.NextSteps = append(rep.NextSteps, "on a server without a desktop login, keep the service running after logout: sudo loginctl enable-linger $USER")
	case "launchd":
		rep.Service.Path = filepath.Join(home, "Library", "LaunchAgents", hermes.LaunchdLabel+".plist")
		so.LogPath = filepath.Join(home, "Library", "Logs", "email-watch.log")
		if rep.Service.Changed, err = hermes.WriteIfChanged(rep.Service.Path, []byte(hermes.LaunchdPlist(so)), 0o644); err != nil {
			return err
		}
		if !start {
			rep.Service.Note = "not started"
			return nil
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = run("launchctl", "bootout", domain+"/"+hermes.LaunchdLabel) // not loaded yet is fine
		if err := run("launchctl", "bootstrap", domain, rep.Service.Path); err != nil {
			rep.Service.Note = err.Error()
			return nil
		}
		rep.Service.Started = true
	case "none":
		rep.Service.Note = "no service manager: run `email watch` yourself (see docs/hermes.md)"
	default:
		return errs.Usage("--service %q: want auto, systemd, launchd or none", kind)
	}
	return nil
}

func writeInstallText(w io.Writer, rep *installReport) error {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	p("Hermes home: %s", rep.HermesHome)
	for _, f := range rep.Files {
		state := "unchanged"
		switch {
		case f.Removed:
			state = "removed"
		case f.Changed:
			state = "written"
		}
		p("  %-9s %s", state, f.Path)
	}
	if rep.Policy.Written {
		p("Policy: wrote %s from the %s template", rep.Policy.Path, rep.Policy.Template)
	} else {
		p("Policy: kept %s", rep.Policy.Path)
	}
	if len(rep.HermesEnv.Added) > 0 {
		p("Hermes .env: added %s", strings.Join(rep.HermesEnv.Added, ", "))
	}
	p("Watch token: %s (0600)", rep.TokenFile)
	svc := rep.Service
	switch {
	case svc.Kind == "none":
		p("Watch service: none (%s)", svc.Note)
	case svc.Started:
		p("Watch service: %s %s, running", svc.Kind, svc.Path)
	default:
		p("Watch service: %s %s, %s", svc.Kind, svc.Path, svc.Note)
	}
	if rep.Cron != nil {
		p("Cron job %q: %s", rep.Cron.Name, rep.Cron.Action)
	}
	p("\nAdd this to %s:\n", filepath.Join(rep.HermesHome, "config.yaml"))
	p("%s", rep.HermesConfig)
	p("Next:")
	for i, s := range rep.NextSteps {
		p("  %d. %s", i+1, s)
	}
	return nil
}
