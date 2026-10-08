package msgraph

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

func readFixture(name string) ([]byte, error) { return os.ReadFile("testdata/" + name) }

func mustMode(s string) policy.Mode {
	m, err := policy.ParseMode(s)
	if err != nil {
		panic(err)
	}
	return m
}

type fakeMSAL struct {
	accounts []public.Account
	err      error
	calls    int
	scopes   []string
}

func (f *fakeMSAL) Accounts(context.Context) ([]public.Account, error) { return f.accounts, nil }

func (f *fakeMSAL) AcquireTokenSilent(_ context.Context, scopes []string, _ ...public.AcquireSilentOption) (public.AuthResult, error) {
	f.calls++
	f.scopes = scopes
	if f.err != nil {
		return public.AuthResult{}, f.err
	}
	return public.AuthResult{AccessToken: "at", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func lockedAccount() *auth.Account {
	return &auth.Account{Alias: "work", Provider: Name, Mode: policy.ModeDraft, Username: "james@tronai.example", HomeAccountID: "uid.utid"}
}

func TestSilentTokenCachesAndUsesModeScopes(t *testing.T) {
	m := &fakeMSAL{accounts: []public.Account{{HomeAccountID: "uid.utid", PreferredUsername: "James@tronai.example"}}}
	s := &silentTokens{msal: m, acct: lockedAccount()}
	for range 3 {
		tok, err := s.Token(ctx)
		if err != nil || tok != "at" {
			t.Fatalf("%q %v", tok, err)
		}
	}
	if m.calls != 1 {
		t.Fatalf("silent calls: %d", m.calls)
	}
	if strings.Join(m.scopes, " ") != "User.Read Mail.Read Mail.ReadWrite" {
		t.Fatalf("scopes: %v", m.scopes)
	}
}

func TestSilentTokenAccountLock(t *testing.T) {
	m := &fakeMSAL{accounts: []public.Account{{HomeAccountID: "uid.utid", PreferredUsername: "intruder@tronai.example"}}}
	_, err := (&silentTokens{msal: m, acct: lockedAccount()}).Token(ctx)
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "account lock") || m.calls != 0 {
		t.Fatalf("err %v calls %d", err, m.calls)
	}
	m = &fakeMSAL{accounts: []public.Account{{HomeAccountID: "someone.else", PreferredUsername: "james@tronai.example"}}}
	if _, err := (&silentTokens{msal: m, acct: lockedAccount()}).Token(ctx); errs.ExitCode(err) != errs.ExitAuth {
		t.Fatalf("wrong home id: %v", err)
	}
}

func TestSilentTokenRefreshFailureIsExit3(t *testing.T) {
	for _, msg := range []string{
		"invalid_grant: AADSTS700082: The refresh token has expired due to inactivity.",
		"interaction_required: AADSTS50076: MFA required",
	} {
		m := &fakeMSAL{accounts: []public.Account{{HomeAccountID: "uid.utid", PreferredUsername: "james@tronai.example"}}, err: errors.New(msg)}
		_, err := (&silentTokens{msal: m, acct: lockedAccount()}).Token(ctx)
		e := errs.From(err)
		if e.Exit != errs.ExitAuth || !strings.Contains(e.Message, "email auth login -a work --provider msgraph --mode draft") ||
			!strings.Contains(e.Message, "AADSTS") {
			t.Fatalf("%s → %+v", msg, e)
		}
	}
}

func TestFileCacheRoundTripPerms(t *testing.T) {
	store := &auth.Store{Dir: t.TempDir()}
	raw, _ := readFixture("msal_cache_dupes.json")
	fc := &fileCache{store: store, alias: "work", homeID: "uid.utid"}
	if err := fc.Export(ctx, bytesMarshaler(raw), cacheHintsExport()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(store.AccountDir("work") + "/" + cacheFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm: %v %v", err, fi.Mode())
	}
	var got []byte
	if err := fc.Replace(ctx, unmarshalerFunc(func(b []byte) error { got = b; return nil }), cacheHintsReplace()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "rt-other") || strings.Contains(string(got), "rt-plain") {
		t.Fatalf("not deduped on load: %s", got)
	}
}

type bytesMarshaler []byte

func (b bytesMarshaler) Marshal() ([]byte, error) { return b, nil }

type unmarshalerFunc func([]byte) error

func (f unmarshalerFunc) Unmarshal(b []byte) error { return f(b) }

func cacheHintsExport() cache.ExportHints   { return cache.ExportHints{} }
func cacheHintsReplace() cache.ReplaceHints { return cache.ReplaceHints{} }

func TestAuthorizeURL(t *testing.T) {
	acct := &auth.Account{Alias: "work", Mode: policy.ModeDraft, TenantID: "97acb66e-0e4a-4325-a2f0-76b5ea22b8d2", ClientID: "ad23d4f7-ab01-4825-93d6-ff2be487f96d"}
	raw := authorizeURL(acct)("http://localhost:51234/callback", "st", "ch")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "login.microsoftonline.com" || u.Path != "/97acb66e-0e4a-4325-a2f0-76b5ea22b8d2/oauth2/v2.0/authorize" {
		t.Fatalf("endpoint: %s", raw)
	}
	q := u.Query()
	want := map[string]string{
		"client_id": acct.ClientID, "response_type": "code", "response_mode": "query",
		"redirect_uri": "http://localhost:51234/callback", "state": "st",
		"code_challenge": "ch", "code_challenge_method": "S256",
		"scope": "openid profile User.Read Mail.Read offline_access Mail.ReadWrite",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if strings.Contains(raw, "+") || strings.Contains(q.Get("scope"), "Mail.Send") {
		t.Fatalf("url: %s", raw)
	}
}

func TestLoginRejectsBadFlows(t *testing.T) {
	store := &auth.Store{Dir: t.TempDir()}
	acct := &auth.Account{Alias: "work", Mode: policy.ModeDraft, ClientID: "c"}
	if _, err := Login(ctx, LoginParams{Store: store, Account: acct, Flow: "implicit"}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("flow: %v", err)
	}
	if _, err := Login(ctx, LoginParams{Store: store, Account: acct, Flow: FlowDevice, Manual: true}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("device+manual: %v", err)
	}
}
