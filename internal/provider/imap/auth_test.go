package imap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

func TestDefaultServers(t *testing.T) {
	s, err := DefaultServers("j@gmail.com", auth.Servers{})
	if err != nil || s != (auth.Servers{IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 465}) {
		t.Fatalf("gmail defaults = %+v, %v", s, err)
	}
	// A Workspace address on Gmail's servers gets Gmail's SMTP too.
	s, _ = DefaultServers("j@corp.example", auth.Servers{IMAPHost: "imap.gmail.com"})
	if s.SMTPHost != "smtp.gmail.com" {
		t.Fatalf("workspace = %+v", s)
	}
	s, _ = DefaultServers("j@example.org", auth.Servers{IMAPHost: "mail.example.org", IMAPPort: 143})
	if s != (auth.Servers{IMAPHost: "mail.example.org", IMAPPort: 143, SMTPPort: 465}) {
		t.Fatalf("generic = %+v", s)
	}
	if _, err := DefaultServers("j@example.org", auth.Servers{}); errs.From(err).Exit != errs.ExitUsage {
		t.Fatalf("generic without host: %v", err)
	}
}

func TestLogin(t *testing.T) {
	srvTLS, cliTLS := testTLS(t)
	f := newFakeGmail(t, gmUser, gmPass)
	host, port := f.start(srvTLS)
	store := &auth.Store{Dir: t.TempDir()}
	ctx := context.Background()
	acct := func(user string) *auth.Account {
		return &auth.Account{Alias: "gmail", Provider: Name, Mode: policy.ModeDraft, Username: user,
			Servers: &auth.Servers{IMAPHost: host, IMAPPort: port}}
	}

	_, err := Login(ctx, LoginParams{Store: store, Account: acct(gmUser), Password: "wrong", TLS: cliTLS})
	if errs.From(err).Exit != errs.ExitAuth || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("bad password: %v", err)
	}
	if b, _ := store.ReadSecret("gmail", credentialsFile); b != nil {
		t.Fatal("credentials stored after a failed login")
	}

	a := acct(gmUser)
	res, err := Login(ctx, LoginParams{Store: store, Account: a, Password: gmPass, TLS: cliTLS})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Gmail || res.Username != gmUser || len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "send") {
		t.Fatalf("login = %+v", res)
	}
	fi, err := os.Stat(filepath.Join(store.AccountDir("gmail"), credentialsFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", fi, err)
	}
	if a.Servers.SMTPPort != 465 {
		t.Fatalf("servers = %+v", a.Servers)
	}
	if err := store.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	if pw, err := LoadCredentials(store, a); err != nil || pw != gmPass {
		t.Fatalf("load = %q, %v", pw, err)
	}
	if b, _ := os.ReadFile(filepath.Join(store.AccountDir("gmail"), "account.json")); strings.Contains(string(b), gmPass) {
		t.Fatal("account.json holds the password")
	}

	// The alias stays locked to its address.
	_, err = Login(ctx, LoginParams{Store: store, Account: acct("other@gmail.com"), Password: gmPass, TLS: cliTLS})
	if errs.From(err).Exit != errs.ExitAuth || !strings.Contains(err.Error(), "account lock") {
		t.Fatalf("lock: %v", err)
	}
	// Send mode: no "only the policy" warning.
	a.Mode = policy.ModeSend
	if res, err = Login(ctx, LoginParams{Store: store, Account: a, Password: gmPass, TLS: cliTLS}); err != nil || len(res.Warnings) != 1 {
		t.Fatalf("send-mode login = %+v, %v", res, err)
	}
}
