package hermes

import (
	"fmt"
	"html"
	"strings"
)

// ServiceOptions describe the `email watch` service for a single host.
type ServiceOptions struct {
	Bin        string   // absolute path of the email binary
	Accounts   []string // -a; empty = every account
	NotifyURL  string
	TokenFile  string // KEY=VALUE file holding EMAIL_WATCH_NOTIFY_TOKEN (0600)
	ConfigDir  string
	StateDir   string
	PolicyFile string // empty = <config dir>/policy.toml
	HealthAddr string
	LogPath    string // launchd only
}

// Service names.
const (
	SystemdUnitName = "email-watch.service"
	LaunchdLabel    = "io.github.james-tronai.email-watch"
	NotifyTokenEnv  = "EMAIL_WATCH_NOTIFY_TOKEN"
)

func (o ServiceOptions) watchArgs(tokenFlag []string) []string {
	args := []string{o.Bin, "watch"}
	for _, a := range o.Accounts {
		args = append(args, "-a", a)
	}
	args = append(args, "--notify-url", o.NotifyURL)
	args = append(args, tokenFlag...)
	args = append(args, "--health-addr", o.HealthAddr)
	return args
}

func (o ServiceOptions) env() [][2]string {
	env := [][2]string{{"EMAIL_CONFIG_DIR", o.ConfigDir}, {"EMAIL_STATE_DIR", o.StateDir}}
	if o.PolicyFile != "" {
		env = append(env, [2]string{"EMAIL_POLICY_FILE", o.PolicyFile})
	}
	return env
}

// systemdQuote quotes one word for ExecStart/Environment and escapes %.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$;") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "$", "$$")
	return `"` + s + `"`
}

// SystemdUnit is a systemd user unit. The token comes from EnvironmentFile;
// the home directory is read-only except for the state directory and the
// account directories (Microsoft 365 and Gmail refresh their tokens there).
func SystemdUnit(o ServiceOptions) string {
	var exec []string
	for _, a := range o.watchArgs([]string{"--notify-token-env", NotifyTokenEnv}) {
		exec = append(exec, systemdQuote(a))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# Written by `+"`email hermes install`"+`; run it again to update. Logs: journalctl --user -u email-watch
[Unit]
Description=email watch: wake Hermes when new mail arrives
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s
EnvironmentFile=%s
`, strings.Join(exec, " "), systemdQuote(o.TokenFile))
	for _, kv := range o.env() {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(kv[0]+"="+kv[1]))
	}
	fmt.Fprintf(&b, `Restart=always
RestartSec=10
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s %s

[Install]
WantedBy=default.target
`, systemdQuote(o.StateDir), systemdQuote(o.ConfigDir+"/accounts"))
	return b.String()
}

// LaunchdPlist is a launchd user agent. launchd has no EnvironmentFile, so
// watch reads the token file itself (--notify-token-file).
func LaunchdPlist(o ServiceOptions) string {
	x := html.EscapeString
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by ` + "`email hermes install`" + `; run it again to update. -->
<plist version="1.0">
<dict>
  <key>Label</key><string>` + LaunchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range o.watchArgs([]string{"--notify-token-file", o.TokenFile}) {
		fmt.Fprintf(&b, "    <string>%s</string>\n", x(a))
	}
	b.WriteString("  </array>\n  <key>EnvironmentVariables</key>\n  <dict>\n")
	for _, kv := range o.env() {
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", x(kv[0]), x(kv[1]))
	}
	fmt.Fprintf(&b, `  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, x(o.LogPath))
	return b.String()
}

// ConfigOptions shape the Hermes config.yaml snippet.
type ConfigOptions struct {
	RouteName string
	CronJob   string
	Port      int
}

// ConfigSnippet is what goes into $HERMES_HOME/config.yaml. Hermes has no
// way for a tool to change its config, so the user merges it by hand.
func ConfigSnippet(o ConfigOptions) string {
	if o.RouteName == "" {
		o.RouteName = DefaultRouteName
	}
	if o.Port == 0 {
		o.Port = DefaultWebhookPort
	}
	return fmt.Sprintf(`# Merge into $HERMES_HOME/config.yaml. The webhook platform itself is turned on
# by WEBHOOK_ENABLED=true and WEBHOOK_SECRET in $HERMES_HOME/.env; do not also
# set platforms.webhook.enabled here.
platforms:
  webhook:
    extra:
      port: %d
      routes:
        %s:
          # New mail from `+"`email watch`"+` runs the %s cron job now, with the new
          # messages as context. Keep this a cron_job route: an agent route counts as
          # a human message, and Hermes replaces its [SILENT] with a warning.
          script: email-summary.py
          cron_job: %s
          prompt: |
            {count} new message(s) in {account_label}, reported by email watch:

            {summary}

            Handle these first (read the important ones with `+"`email {account_flag} get <id> --format text`"+`), then run the usual sync.
skills:
  # Hermes's bundled mail skills compete for the same requests and send the model
  # looking for Google tokens or himalaya instead of `+"`email`"+`.
  disabled: [google-workspace, himalaya, email-inbox-triage]
  auto_load: [email]
`, o.Port, o.RouteName, o.CronJob, o.CronJob)
}
