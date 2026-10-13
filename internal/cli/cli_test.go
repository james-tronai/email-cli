package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type env struct {
	t   *testing.T
	cfg string
}

func newEnv(t *testing.T, policyBody string) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg")
	t.Setenv("EMAIL_CONFIG_DIR", cfg)
	t.Setenv("EMAIL_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("EMAIL_POLICY_FILE", "")
	t.Setenv("EMAIL_ACCOUNT", "")
	if policyBody != "" {
		if err := os.MkdirAll(cfg, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg, "policy.toml"), []byte(policyBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, cfg: cfg}
	e.ok("auth", "login", "-a", "demo", "--provider", "fake", "--mode", "draft", "--username", "james@tronai.com.au")
	return e
}

func (e *env) run(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Execute(BuildInfo{Version: "test"}, args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func (e *env) ok(args ...string) string {
	e.t.Helper()
	code, out, errOut := e.run("", args...)
	if code != 0 {
		e.t.Fatalf("%v: exit %d: %s", args, code, errOut)
	}
	return out
}

func (e *env) code(want int, args ...string) map[string]any {
	e.t.Helper()
	code, _, errOut := e.run("", args...)
	if code != want {
		e.t.Fatalf("%v: exit %d, want %d: %s", args, code, want, errOut)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(errOut), &m)
	return m
}

const draftPolicy = "[default]\nmode = \"draft\"\n"

func TestExitCodes(t *testing.T) {
	e := newEnv(t, draftPolicy)
	e.code(2, "get")
	e.code(2, "get", "")
	e.code(2, "draft", "delete", " ")
	e.code(2, "attachments", "list", "")
	e.code(2, "list", "--bogus")
	e.code(2, "nonsense")
	e.code(2, "list", "--format", "xml")
	e.code(3, "-a", "missing", "list")
	e.code(4, "send", "x")
	e.code(4, "move", "fake-0001", "--to", "trash")
	e.code(5, "get", "does-not-exist")
	if m := e.code(4, "send", "x"); m["error"].(map[string]any)["code"] != "policy_denied" {
		t.Fatalf("error json: %v", m)
	}
}

func TestBuiltinPolicyIsReadOnly(t *testing.T) {
	e := newEnv(t, "")
	e.ok("list")
	e.code(4, "draft", "create", "--to", "a@b.com", "--subject", "s", "--body", "x")
}

func TestBrokenPolicyStopsEverything(t *testing.T) {
	e := newEnv(t, "[default]\nmode = \"draft\"\ntypo = 1\n")
	e.code(4, "list")
}

func TestDraftFlowWithStdinBody(t *testing.T) {
	e := newEnv(t, draftPolicy)
	code, out, errOut := e.run("Body from stdin", "draft", "create", "--to", "Alice <a@tronai.com.au>, b@tronai.com.au", "--subject", "Hi", "--body", "-", "--jq", ".id")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	id := strings.TrimSpace(out)
	got := e.ok("draft", "get", id, "--jq", ".body_text")
	if !strings.HasPrefix(got, "Body from stdin") || !strings.Contains(got, "automated assistant") {
		t.Fatalf("body: %q", got)
	}
	if to := e.ok("draft", "get", id, "--jq", "[.to[].email]|join(\",\")"); strings.TrimSpace(to) != "a@tronai.com.au,b@tronai.com.au" {
		t.Fatalf("to: %q", to)
	}
	e.ok("draft", "update", id, "--subject", "Hi again")
	if s := e.ok("draft", "get", id, "--jq", ".subject"); strings.TrimSpace(s) != "Hi again" {
		t.Fatalf("subject: %q", s)
	}
	if n := e.ok("draft", "list", "--jq", ".messages|length"); strings.TrimSpace(n) != "1" {
		t.Fatalf("drafts: %q", n)
	}
	e.ok("draft", "delete", id)
	e.code(5, "draft", "get", id)
}

func TestDryRunWritesNothing(t *testing.T) {
	e := newEnv(t, draftPolicy)
	out := e.ok("--dry-run", "draft", "create", "--to", "a@tronai.com.au", "--subject", "s", "--body", "x")
	if !strings.Contains(out, `"dry_run": true`) {
		t.Fatalf("out: %s", out)
	}
	if n := e.ok("draft", "list", "--jq", ".messages|length"); strings.TrimSpace(n) != "0" {
		t.Fatalf("dry run created draft: %s", n)
	}
}

func TestTextFormatFencesBody(t *testing.T) {
	e := newEnv(t, draftPolicy)
	out := e.ok("get", "fake-0002", "--format", "text")
	if !strings.Contains(out, "----- BEGIN UNTRUSTED MESSAGE BODY (id=fake-0002) -----") || strings.Contains(out, "attacker@evil.example") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestPaginationCursor(t *testing.T) {
	e := newEnv(t, draftPolicy)
	first := e.ok("list", "--limit", "2", "--jq", ".next_cursor")
	cur := strings.TrimSpace(first)
	if cur == "" || cur == "null" {
		t.Fatalf("no cursor: %q", first)
	}
	if n := e.ok("list", "--limit", "2", "--cursor", cur, "--jq", ".messages|length"); strings.TrimSpace(n) != "1" {
		t.Fatalf("second page: %q", n)
	}
}

func TestCapabilitiesFollowMode(t *testing.T) {
	e := newEnv(t, "[default]\nmode = \"read\"\n")
	out := e.ok("capabilities", "--jq", ".commands|join(\",\")")
	if strings.Contains(out, "draft create") || strings.Contains(out, "send") || !strings.Contains(out, "get") {
		t.Fatalf("commands: %s", out)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t, draftPolicy)
	e.ok("auth", "logout", "-a", "demo")
	e.code(3, "list")
}

func TestWriteResultsCarryTopLevelWebLink(t *testing.T) {
	e := newEnv(t, draftPolicy)
	for _, args := range [][]string{
		{"draft", "create", "--to", "a@tronai.com.au", "--subject", "s", "--body", "x"},
		{"draft", "reply", "fake-0001", "--body", "x"},
		{"draft", "forward", "fake-0001", "--to", "colleague@tronai.com.au"},
	} {
		out := strings.TrimSpace(e.ok(append(args, "--jq", ".web_link")...))
		if !strings.HasPrefix(out, "https://mail.example.test/#/fake-") {
			t.Errorf("%v: web_link %q", args, out)
		}
	}
	id := strings.TrimSpace(e.ok("draft", "list", "--jq", ".messages[0].id"))
	if out := strings.TrimSpace(e.ok("draft", "update", id, "--subject", "t", "--jq", ".web_link")); out == "" || out == "null" {
		t.Errorf("update web_link: %q", out)
	}
	if out := strings.TrimSpace(e.ok("list", "--limit", "1", "--jq", ".has_more")); out != "true" {
		t.Errorf("has_more: %q", out)
	}
	if out := strings.TrimSpace(e.ok("list", "--limit", "50", "--jq", ".has_more")); out != "false" {
		t.Errorf("has_more (all fit): %q", out)
	}
}

func TestGmailLoginFlagChecks(t *testing.T) {
	e := newEnv(t, draftPolicy)
	for _, args := range [][]string{
		{"auth", "login", "-a", "gw", "--provider", "gmail", "--tenant", "x", "--client-id", "i", "--client-secret", "s"},
		{"auth", "login", "-a", "gw", "--provider", "gmail", "--flow", "device"},
		{"auth", "login", "-a", "gw", "--provider", "gmail", "--client-id", "i"},
		{"auth", "login", "-a", "gw", "--provider", "gmail", "--client-file", "/nonexistent.json"},
		{"auth", "login", "-a", "gw", "--provider", "gmail"}, // no client given or stored
	} {
		e.code(2, args...)
	}
	// A gmail alias that was never logged in has no token: exit 3, not a crash.
	if err := os.MkdirAll(filepath.Join(e.cfg, "accounts", "gw"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.cfg, "accounts", "gw", "account.json"),
		[]byte(`{"alias":"gw","provider":"gmail","mode":"draft","username":"james@tronai.com.au"}`), 0o600)
	e.code(3, "list", "-a", "gw")
}

func TestSendNeedsSendModeAndASender(t *testing.T) {
	e := newEnv(t, draftPolicy)
	e.code(4, "send", "fake-0001") // policy draft
	e2 := newEnv(t, "[default]\nmode = \"send\"\n")
	e2.code(4, "send", "fake-0001") // the account logged in with --mode draft
	e2.ok("auth", "login", "-a", "demo", "--provider", "fake", "--mode", "send", "--username", "james@tronai.com.au")
	if m := e2.code(2, "send", "fake-0001"); !strings.Contains(m["error"].(map[string]any)["message"].(string), "does not support send") {
		t.Fatalf("fake has no sender: %v", m)
	}
}

func TestIMAPLoginFlagChecks(t *testing.T) {
	e := newEnv(t, draftPolicy)
	for _, args := range [][]string{
		{"auth", "login", "-a", "gm", "--provider", "imap", "--username", "j@gmail.com"},                                     // no --password-stdin
		{"auth", "login", "-a", "gm", "--provider", "imap", "--password-stdin"},                                              // no --username
		{"auth", "login", "-a", "gm", "--provider", "imap", "--username", "j@gmail.com", "--password", "x"},                  // never on argv
		{"auth", "login", "-a", "gm", "--provider", "imap", "--username", "j@example.org", "--password-stdin"},               // no host for a non-Gmail domain
		{"auth", "login", "-a", "gm", "--provider", "imap", "--username", "j@gmail.com", "--password-stdin", "--manual"},     // OAuth flag
		{"auth", "login", "-a", "gw", "--provider", "gmail", "--password-stdin", "--client-id", "i", "--client-secret", "s"}, // imap flag elsewhere
		{"auth", "login", "-a", "gw", "--provider", "msgraph", "--imap-host", "imap.example.org"},                            // imap flag elsewhere
		{"auth", "login", "-a", "demo", "--provider", "imap", "--username", "j@gmail.com", "--password-stdin"},               // alias is a fake account
	} {
		code, _, errOut := e.run("app-password\n", args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", args, code, errOut)
		}
		if strings.Contains(errOut, "app-password") {
			t.Errorf("%v: error echoes the password: %s", args, errOut)
		}
	}
}

// An imap account in draft mode: capabilities and the send gate work
// without connecting, auth status never shows the password, and a broken
// credentials file is a login error.
func TestIMAPAccountWithoutNetwork(t *testing.T) {
	e := newEnv(t, draftPolicy)
	dir := filepath.Join(e.cfg, "accounts", "gm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "account.json"), []byte(`{"alias":"gm","provider":"imap","mode":"draft","username":"j@gmail.com",
		"servers":{"imap_host":"127.0.0.1","imap_port":1,"smtp_host":"127.0.0.1","smtp_port":1}}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "imap-credentials.json"), []byte(`{"username":"j@gmail.com","password":"s3cret-app-pw"}`), 0o600)

	var caps struct{ Capabilities, Commands []string }
	if err := json.Unmarshal([]byte(e.ok("-a", "gm", "capabilities")), &caps); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(caps.Capabilities, "send") || slices.Contains(caps.Commands, "send") ||
		!slices.Contains(caps.Commands, "draft create") || !slices.Contains(caps.Commands, "label") {
		t.Errorf("capabilities in draft mode = %+v", caps)
	}
	e.code(4, "-a", "gm", "send", "abc")
	if st := e.ok("auth", "status", "-a", "gm"); strings.Contains(st, "s3cret") || !strings.Contains(st, `"imap_host": "127.0.0.1"`) {
		t.Errorf("auth status: %s", st)
	}

	// The credentials belong to another address: the account lock trips.
	_ = os.WriteFile(filepath.Join(dir, "imap-credentials.json"), []byte(`{"username":"other@gmail.com","password":"x"}`), 0o600)
	e.code(3, "-a", "gm", "list")
	_ = os.Remove(filepath.Join(dir, "imap-credentials.json"))
	e.code(3, "-a", "gm", "list")
}

func TestThreadIsAnObject(t *testing.T) {
	e := newEnv(t, draftPolicy)
	var pg struct {
		Messages []struct {
			ID       string `json:"id"`
			ThreadID string `json:"thread_id"`
		}
	}
	if err := json.Unmarshal([]byte(e.ok("list", "--limit", "1")), &pg); err != nil || len(pg.Messages) != 1 {
		t.Fatalf("%+v %v", pg, err)
	}
	tid := pg.Messages[0].ThreadID
	var th struct {
		ThreadID string `json:"thread_id"`
		Messages []struct{ ID string }
	}
	if err := json.Unmarshal([]byte(e.ok("thread", tid)), &th); err != nil || th.ThreadID != tid || len(th.Messages) == 0 {
		t.Fatalf("thread: %+v %v", th, err)
	}
	if got := strings.TrimSpace(e.ok("thread", tid, "--jq", ".messages[0].id")); got != th.Messages[0].ID {
		t.Fatalf("--jq .messages: %s", got)
	}
	if got := strings.TrimSpace(e.ok("thread", tid, "--format", "id")); got != th.Messages[0].ID {
		t.Fatalf("--format id: %q", got)
	}
}

func TestAuthStatusShowsEffectiveIMAPServers(t *testing.T) {
	e := newEnv(t, draftPolicy)
	dir := filepath.Join(e.cfg, "accounts", "gm")
	_ = os.MkdirAll(dir, 0o700)
	// Logged in with Gmail's defaults (ports left out).
	_ = os.WriteFile(filepath.Join(dir, "account.json"), []byte(`{"alias":"gm","provider":"imap","mode":"draft","username":"j@gmail.com",
		"servers":{"imap_host":"imap.gmail.com","imap_port":0}}`), 0o600)
	var st []map[string]any
	if err := json.Unmarshal([]byte(e.ok("auth", "status", "-a", "gm")), &st); err != nil {
		t.Fatal(err)
	}
	if st[0]["imap_server"] != "imap.gmail.com:993" || st[0]["smtp_server"] != "smtp.gmail.com:465" {
		t.Fatalf("status %v", st[0])
	}
	if txt := e.ok("auth", "status", "-a", "gm", "--format", "text"); !strings.Contains(txt, "imap=imap.gmail.com:993 smtp=smtp.gmail.com:465") {
		t.Fatalf("text: %s", txt)
	}
	if st := e.ok("auth", "status", "-a", "demo"); strings.Contains(st, "imap_server") {
		t.Fatalf("non-imap account shows servers: %s", st)
	}
}

func TestWatchFlagChecks(t *testing.T) {
	e := newEnv(t, draftPolicy)
	e.code(2, "watch", "-a", "demo")                                               // no --notify-url
	e.code(2, "watch", "-a", "demo", "--notify-url", "ftp://x")                    // not http
	e.code(2, "watch", "-a", "demo", "--dry-run", "--health-addr", "0.0.0.0:8645") // not loopback
	e.code(2, "watch", "-a", "demo", "--notify-url", "http://127.0.0.1:1/", "--notify-token-env", "EMAIL_TEST_UNSET_TOKEN")
	e.code(2, "watch", "-a", "demo", "--dry-run", "--poll-interval", "1s")
	e.code(3, "watch", "-a", "missing", "--dry-run")
	e.code(2, "watch", "healthz", "--health-addr", "10.0.0.1:8645")
}

// End to end on the fake provider: the watch cursor starts before the
// seeded mail, so the first sync finds it; [watch] drops one sender and
// the rest goes out as one notification (printed by --dry-run). Logs are
// JSON lines; the audit has the notification.
func TestWatchDryRunEndToEnd(t *testing.T) {
	e := newEnv(t, draftPolicy+"\n[watch]\nignore_senders = [\"Billing@\"]\n")
	stateDir := filepath.Join(t.TempDir(), "watch")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "demo.json"), []byte(`{"version":1,"provider":"fake","cursor":"0","notified":[]}`), 0o600)

	code, out, errOut := e.run("", "watch", "-a", "demo", "--dry-run", "--debounce", "0s", "--poll-interval", "5s",
		"--state-dir", stateDir, "--timeout", "1500ms", "--log-level", "debug")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var p struct {
		Source, Account string
		Count           int
		Messages        []struct {
			Subject string
			From    struct{ Email string }
		}
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if p.Source != "email-watch" || p.Account != "demo" || p.Count != 2 || len(p.Messages) != 2 {
		t.Fatalf("payload %+v", p)
	}
	for _, m := range p.Messages {
		if m.From.Email == "billing@vendor.example" {
			t.Fatal("ignore_senders did not apply")
		}
	}
	events := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(errOut), "\n") {
		var l map[string]any
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if l["ts"] == nil || l["level"] == nil || l["msg"] == nil {
			t.Fatalf("log line %v", l)
		}
		ev, _ := l["event"].(string)
		events[ev] = true
	}
	for _, ev := range []string{"start", "new_mail", "filtered", "notify_ok", "stop"} {
		if !events[ev] {
			t.Errorf("no %s event in %s", ev, errOut)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(os.Getenv("EMAIL_STATE_DIR"), "audit.jsonl"))
	if !strings.Contains(string(audit), `"op":"watch.notify"`) || !strings.Contains(string(audit), `"count":2`) {
		t.Fatalf("audit: %s", audit)
	}
	// `email sync` keeps its own cursor: watch did not create or move it.
	if _, err := os.Stat(filepath.Join(os.Getenv("EMAIL_STATE_DIR"), "sync")); err == nil {
		t.Fatal("watch touched the sync state")
	}
}

func TestUnknownSubcommand(t *testing.T) {
	e := newEnv(t, draftPolicy)
	e.code(2, "accounts", "bogus")
	e.code(2, "draft", "lst")
	e.code(2, "bogus")
	e.code(0, "accounts")
}
