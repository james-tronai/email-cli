package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
