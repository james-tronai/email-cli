package auth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

func TestAccountRoundTripAndPerms(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	a := &Account{Alias: "work", Provider: "msgraph", Mode: policy.ModeDraft, Username: "james@tronai.com.au"}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSecret("work", "msal_cache.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadAccount("work")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != policy.ModeDraft || got.Domain() != "tronai.com.au" {
		t.Fatalf("got %+v", got)
	}
	for _, p := range []string{"account.json", "msal_cache.json"} {
		fi, err := os.Stat(filepath.Join(s.AccountDir("work"), p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perm %o", p, fi.Mode().Perm())
		}
	}
	fi, _ := os.Stat(s.AccountDir("work"))
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir perm %o", fi.Mode().Perm())
	}
}

func TestLoadMissingAccountIsAuthError(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	_, err := s.LoadAccount("nope")
	if errs.ExitCode(err) != errs.ExitAuth {
		t.Fatalf("want exit 3, got %v", err)
	}
}

func TestValidateAliasRejectsTraversal(t *testing.T) {
	for _, a := range []string{"../x", "a/b", "", "Work", ".hidden"} {
		if ValidateAlias(a) == nil {
			t.Errorf("alias %q accepted", a)
		}
	}
}

func TestResolveAlias(t *testing.T) {
	t.Setenv("EMAIL_ACCOUNT", "")
	s := &Store{Dir: t.TempDir()}
	if _, err := s.ResolveAlias(""); errs.ExitCode(err) != errs.ExitAuth {
		t.Fatalf("no accounts: %v", err)
	}
	_ = s.SaveAccount(&Account{Alias: "one", Provider: "fake", Mode: policy.ModeRead})
	if a, _ := s.ResolveAlias(""); a != "one" {
		t.Fatalf("single account: %q", a)
	}
	_ = s.SaveAccount(&Account{Alias: "two", Provider: "fake", Mode: policy.ModeRead})
	if _, err := s.ResolveAlias(""); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("ambiguous: %v", err)
	}
	_ = s.SaveConfig(&Config{DefaultAccount: "two"})
	if a, _ := s.ResolveAlias(""); a != "two" {
		t.Fatalf("default: %q", a)
	}
	if a, _ := s.ResolveAlias("one"); a != "one" {
		t.Fatalf("flag: %q", a)
	}
}

func TestCheckIdentity(t *testing.T) {
	a := &Account{Alias: "work", Username: "James@tronai.com.au"}
	if err := CheckIdentity(a, "james@tronai.com.au"); err != nil {
		t.Fatal(err)
	}
	if err := CheckIdentity(a, "someone@else.com"); errs.ExitCode(err) != errs.ExitAuth {
		t.Fatalf("want auth error, got %v", err)
	}
}
