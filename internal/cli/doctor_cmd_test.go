package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/james-tronai/email-cli/internal/auth/oauthflow"
)

func parseDoctor(t *testing.T, out string) doctorReport {
	t.Helper()
	var r doctorReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return r
}

func find(r doctorReport, name, account string) check {
	for _, c := range r.Checks {
		if c.Name == name && c.Account == account {
			return c
		}
	}
	return check{}
}

func TestDoctorPassesOnAFreshInstall(t *testing.T) {
	e := newEnv(t, draftPolicy)
	var gets, posts atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		} else {
			gets.Add(1)
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer hook.Close()

	out := e.ok("doctor", "--no-update-check", "--notify-url", hook.URL+"/webhooks/email")
	r := parseDoctor(t, out)
	if !r.OK {
		t.Fatalf("not ok: %s", out)
	}
	if c := find(r, "login", "demo"); c.Status != checkOK || !strings.Contains(c.Detail, "1 message listed") {
		t.Fatalf("login %+v", c)
	}
	if c := find(r, "policy", ""); c.Status != checkOK || !strings.Contains(c.Detail, "mode draft") {
		t.Fatalf("policy %+v", c)
	}
	if c := find(r, "watch state", "demo"); c.Status != checkSkip {
		t.Fatalf("watch state %+v", c)
	}
	if c := find(r, "webhook", ""); c.Status != checkOK || gets.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("webhook %+v gets %d posts %d", c, gets.Load(), posts.Load())
	}
	// Nothing from the mailbox is printed: the seeded subjects never appear.
	for _, s := range []string{"Lunch", "Invoice", "Design review", "alice@"} {
		if strings.Contains(out, s) {
			t.Fatalf("doctor printed mail content %q", s)
		}
	}
	if txt := e.ok("doctor", "--no-update-check", "--format", "text"); !strings.Contains(txt, "[ok  ] demo: login") || !strings.Contains(txt, "all checks passed") {
		t.Fatalf("text: %s", txt)
	}
}

func TestDoctorFailures(t *testing.T) {
	e := newEnv(t, "")
	// No policy file: a warning, not a failure.
	r := parseDoctor(t, e.ok("doctor", "--no-update-check"))
	if c := find(r, "policy", ""); c.Status != checkWarn || !strings.Contains(c.Fix, "docs/policy/draft.toml") {
		t.Fatalf("policy %+v", c)
	}
	// A broken policy and a missing account fail, with exit 1.
	_ = os.WriteFile(filepath.Join(e.cfg, "policy.toml"), []byte("[default]\nmode = \"draft\"\nbogus = 1\n"), 0o600)
	code, out, _ := e.run("", "doctor", "--no-update-check", "-a", "demo", "-a", "nosuch")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	r = parseDoctor(t, out)
	if r.OK || find(r, "policy", "").Status != checkFail || find(r, "login", "nosuch").Status != checkFail {
		t.Fatalf("%s", out)
	}
}

func TestDoctorReadsWatchState(t *testing.T) {
	e := newEnv(t, draftPolicy)
	dir := filepath.Join(t.TempDir(), "watch")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "demo.json"), []byte(`{"version":1,"provider":"fake","cursor":"3","notified":["a","b"],"pending":[{"id":"c","queued_at":"2026-10-10T00:00:00Z"}]}`), 0o600)
	r := parseDoctor(t, e.ok("doctor", "--no-update-check", "--state-dir", dir))
	if c := find(r, "watch state", "demo"); c.Status != checkOK || !strings.Contains(c.Detail, "2 notified, 1 waiting") {
		t.Fatalf("%+v", c)
	}
}

func TestIMAPPresetFlags(t *testing.T) {
	e := newEnv(t, draftPolicy)
	for _, args := range [][]string{
		{"--preset", "outlook", "--username", "j@corp.example"},
		{"--username", "j@hotmail.com"},
		{"--preset", "mailu", "--username", "j@corp.example"},
		{"--preset", "nope", "--username", "j@corp.example"},
	} {
		code, _, errOut := e.run("pw\n", append([]string{"auth", "login", "-a", "x", "--provider", "imap", "--password-stdin"}, args...)...)
		if code != 2 {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	// --preset and --host are imap-only.
	e.code(2, "auth", "login", "-a", "y", "--provider", "msgraph", "--client-id", "x", "--preset", "gmail")
	e.code(2, "auth", "login", "-a", "y", "--provider", "fake", "--host", "mail.example")
}

func TestPolicyTemplatesLoad(t *testing.T) {
	files, _ := filepath.Glob("../../docs/policy/*.toml")
	files = append(files, "../../docs/policy.example.toml")
	if len(files) < 5 {
		t.Fatalf("templates: %v", files)
	}
	for _, f := range files {
		e := newEnv(t, "")
		b, _ := os.ReadFile(f)
		_ = os.WriteFile(filepath.Join(e.cfg, "policy.toml"), b, 0o600)
		if out := e.ok("policy", "show"); !strings.Contains(out, `"mode"`) {
			t.Errorf("%s: %s", f, out)
		}
	}
}

func TestManualLoginPromptIsPlainText(t *testing.T) {
	var out, errb strings.Builder
	a := &app{stdout: &out, stderr: &errb, format: "json"}
	if err := a.loginPrompt(oauthflow.Prompt{Mode: "manual", AuthorizeURL: "https://login.example/authorize?x=1", RedirectURI: "http://localhost:1/callback"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "Copy the whole address") || !strings.Contains(errb.String(), "https://login.example/authorize?x=1") {
		t.Fatalf("stderr: %s", errb.String())
	}
	// Not a terminal: scripts keep getting the JSON prompt on stdout.
	if !strings.Contains(out.String(), `"authorize_url"`) {
		t.Fatalf("stdout: %s", out.String())
	}
}
