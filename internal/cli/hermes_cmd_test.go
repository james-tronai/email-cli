package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHermesInstallSingleHost(t *testing.T) {
	e := newEnv(t, "")
	home := filepath.Join(t.TempDir(), "hermes")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, ".env"), []byte("WEBHOOK_SECRET=s3cret\n"), 0o600)
	t.Setenv("EMAIL_WATCH_NOTIFY_TOKEN", "")
	out := e.ok("hermes", "install", "--hermes-home", home, "--service", "none", "--format", "json")
	var rep installReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 2 || !rep.Policy.Written || rep.Policy.Template != "draft" || rep.Service.Kind != "none" ||
		!strings.Contains(rep.HermesConfig, "cron_job: email-triage") || rep.CronNote == "" {
		t.Fatalf("%+v", rep)
	}
	if strings.Join(rep.HermesEnv.Added, ",") != "WEBHOOK_ENABLED" {
		t.Fatalf("added %v", rep.HermesEnv.Added)
	}
	tok, _ := os.ReadFile(rep.TokenFile)
	if st, _ := os.Stat(rep.TokenFile); string(tok) != "EMAIL_WATCH_NOTIFY_TOKEN=s3cret\n" || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file %q", tok)
	}
	if got, err := readTokenFile("notify-token-file", rep.TokenFile, "EMAIL_WATCH_NOTIFY_TOKEN"); err != nil || got != "s3cret" {
		t.Fatalf("readTokenFile %q %v", got, err)
	}
	pol, _ := os.ReadFile(rep.Policy.Path)
	if !strings.Contains(string(pol), `mode = "draft"`) {
		t.Fatalf("policy %s", pol)
	}
	// Again: nothing changes, the policy is kept, .env is not appended twice.
	out = e.ok("hermes", "install", "--hermes-home", home, "--service", "none", "--format", "json")
	rep = installReport{}
	json.Unmarshal([]byte(out), &rep)
	if rep.Policy.Written || len(rep.HermesEnv.Added) != 0 || rep.Files[0].Changed {
		t.Fatalf("second run: %+v", rep)
	}
	if env, _ := os.ReadFile(filepath.Join(home, ".env")); strings.Count(string(env), "WEBHOOK_ENABLED") != 1 {
		t.Fatalf(".env %s", env)
	}
	e.code(2, "hermes", "install", "--hermes-home", filepath.Join(home, "missing"), "--service", "none")
	e.code(2, "hermes", "install", "--hermes-home", home, "--service", "none", "--policy-template", "nope", "--format", "json")
}

func TestReadTokenFile(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{
		"abc==\n":                               "abc==",
		"# c\nEMAIL_WATCH_NOTIFY_TOKEN='x y'\n": "x y",
		"OTHER=1\nEMAIL_WATCH_NOTIFY_TOKEN=t\n": "t",
	} {
		p := filepath.Join(dir, "f")
		os.WriteFile(p, []byte(body), 0o600)
		if got, err := readTokenFile("notify-token-file", p, "EMAIL_WATCH_NOTIFY_TOKEN"); err != nil || got != want {
			t.Errorf("%q: %q %v", body, got, err)
		}
	}
	p := filepath.Join(dir, "g")
	os.WriteFile(p, []byte("OTHER=1\n"), 0o600)
	if _, err := readTokenFile("notify-token-file", p, "EMAIL_WATCH_NOTIFY_TOKEN"); err == nil {
		t.Error("file without the key accepted")
	}
}

func TestSelfInstallWrapper(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	code, out, errOut := runWithVersion(t, []string{"self-install", "--dest", dest,
		"--env", "EMAIL_POLICY_FILE=/etc/email/policy.toml", "--env", "EMAIL_CONFIG_DIR=/data/.config/email"}, "v1")
	if code != 0 {
		t.Fatal(errOut)
	}
	var res selfInstallResult
	json.Unmarshal([]byte(out), &res)
	lib := filepath.Join(filepath.Dir(dest), "lib", "email-cli", "email")
	if res.Path != filepath.Join(dest, "email") || res.Binary != lib || res.Unchanged {
		t.Fatalf("%+v", res)
	}
	w, _ := os.ReadFile(res.Path)
	want := "export EMAIL_POLICY_FILE='/etc/email/policy.toml'\nexport EMAIL_CONFIG_DIR='/data/.config/email'\nexec '" + lib + "' \"$@\"\n"
	if !strings.HasPrefix(string(w), "#!/bin/sh\n") || !strings.HasSuffix(string(w), want) {
		t.Fatalf("wrapper:\n%s", w)
	}
	if st, _ := os.Stat(res.Path); st.Mode().Perm() != 0o755 {
		t.Fatal(st.Mode())
	}
	_, out, _ = runWithVersion(t, []string{"self-install", "--dest", dest, "--env", "EMAIL_POLICY_FILE=/etc/email/policy.toml",
		"--env", "EMAIL_CONFIG_DIR=/data/.config/email"}, "v1")
	json.Unmarshal([]byte(out), &res)
	if !res.Unchanged {
		t.Fatal("second run changed something")
	}
	for _, bad := range []string{"1X=y", "K=it's"} {
		if code, _, _ := runWithVersion(t, []string{"self-install", "--dest", dest, "--env", bad}, "v1"); code != 2 {
			t.Errorf("%q accepted", bad)
		}
	}
}
