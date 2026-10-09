package gmail

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/auth/oauthflow"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

var testClient = &Client{ID: "123-abc.apps.googleusercontent.com", Secret: "GOCSPX-not-really-secret"}

// login runs a --manual login against the fake server: the "user" pastes
// the redirect URL as soon as the prompt appears.
func login(t *testing.T, f *fakeGmail, store *auth.Store, acct *auth.Account) (*LoginResult, *oauthflow.Prompt, error) {
	t.Helper()
	pr, pw := io.Pipe()
	var prompt oauthflow.Prompt
	res, err := Login(ctx, LoginParams{
		Store: store, Account: acct, Client: testClient,
		Endpoint: oauth2.Endpoint{AuthURL: f.srv.URL + "/o/oauth2/v2/auth", TokenURL: f.srv.URL + "/token", AuthStyle: oauth2.AuthStyleInParams},
		BaseURL:  f.srv.URL, Manual: true, Stdin: pr,
		OnPrompt: func(p oauthflow.Prompt) error {
			prompt = p
			au, _ := url.Parse(p.AuthorizeURL)
			go func() {
				_, _ = io.WriteString(pw, p.RedirectURI+"?state="+au.Query().Get("state")+"&code=4/auth-code&scope=x\n")
			}()
			return nil
		},
	})
	return res, &prompt, err
}

func TestLoginManual(t *testing.T) {
	f := newFakeGmail(t)
	f.on(route{method: "POST", path: "/token", fixture: "token.json"})
	f.on(route{method: "GET", path: u + "/profile", fixture: "profile.json"})
	store := &auth.Store{Dir: t.TempDir()}
	acct := &auth.Account{Alias: "gw", Provider: Name, Mode: policy.ModeDraft}

	res, prompt, err := login(t, f, store, acct)
	if err != nil {
		t.Fatal(err)
	}
	au, _ := url.Parse(prompt.AuthorizeURL)
	v := au.Query()
	if v.Get("scope") != scopeModify || v.Get("access_type") != "offline" || v.Get("prompt") != "consent select_account" ||
		v.Get("code_challenge_method") != "S256" || v.Get("client_id") != testClient.ID || v.Has("include_granted_scopes") {
		t.Fatalf("authorize url: %v", v)
	}
	if strings.Contains(au.RawQuery, "+") || !strings.HasPrefix(v.Get("redirect_uri"), "http://localhost:") {
		t.Fatalf("authorize url encoding/redirect: %s", au.RawQuery)
	}
	tr := q(t, f.one("POST", "/token").Body)
	if tr.Get("code") != "4/auth-code" || tr.Get("code_verifier") == "" || tr.Get("client_secret") != testClient.Secret ||
		tr.Get("redirect_uri") != v.Get("redirect_uri") {
		t.Fatalf("token request: %v", tr)
	}
	if res.Username != "james@tronai.example" || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "can also send") {
		t.Fatalf("result: %+v", res)
	}
	path := filepath.Join(store.AccountDir("gw"), tokenFile)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", err, fi)
	}
	var st storedToken
	b, _ := os.ReadFile(path)
	if json.Unmarshal(b, &st) != nil || st.Token.RefreshToken != "1//refresh-token" || st.IssuedAt.IsZero() {
		t.Fatalf("stored token: %s", b)
	}
	if c, _ := LoadClient(store, "gw"); c == nil || c.Secret != testClient.Secret {
		t.Fatalf("client not stored: %+v", c)
	}
}

func TestLoginRejectsMissingScopeAndLock(t *testing.T) {
	f := newFakeGmail(t)
	f.on(route{method: "POST", path: "/token", fixture: "token_readonly_only.json", times: 1})
	f.on(route{method: "POST", path: "/token", fixture: "token.json"})
	f.on(route{method: "GET", path: u + "/profile", fixture: "profile.json"})
	store := &auth.Store{Dir: t.TempDir()}

	_, _, err := login(t, f, store, &auth.Account{Alias: "gw", Provider: Name, Mode: policy.ModeDraft})
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "did not grant") {
		t.Fatalf("unticked scope: %v", err)
	}
	_, _, err = login(t, f, store, &auth.Account{Alias: "gw", Provider: Name, Mode: policy.ModeDraft, Username: "other@tronai.example"})
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "account lock") {
		t.Fatalf("lock: %v", err)
	}
	if b, _ := store.ReadSecret("gw", tokenFile); b != nil {
		t.Fatal("a failed login must not write a token")
	}
	if _, err := Login(ctx, LoginParams{Store: store, Account: &auth.Account{Alias: "gw", Mode: policy.ModeRead}}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("no client: %v", err)
	}
}

func writeToken(t *testing.T, store *auth.Store, tok *oauth2.Token) {
	t.Helper()
	if err := SaveClient(store, "gw", testClient); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(storedToken{Token: tok, Scopes: []string{scopeModify}, IssuedAt: time.Now()})
	if err := store.WriteSecret("gw", tokenFile, b); err != nil {
		t.Fatal(err)
	}
}

func TestTokenRefreshPersistsAndChecksLock(t *testing.T) {
	f := newFakeGmail(t)
	f.on(route{method: "POST", path: "/token", fixture: "token.json"})
	f.on(route{method: "GET", path: u + "/profile", fixture: "profile.json"})
	ep := oauth2.Endpoint{TokenURL: f.srv.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
	store := &auth.Store{Dir: t.TempDir()}
	expired := &oauth2.Token{AccessToken: "old", RefreshToken: "1//keep-me", Expiry: time.Now().Add(-time.Hour)}

	writeToken(t, store, expired)
	acct := &auth.Account{Alias: "gw", Provider: Name, Mode: policy.ModeDraft, Username: "james@tronai.example"}
	ts, err := NewTokenSource(TokenSourceParams{Store: store, Account: acct, Endpoint: ep, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Token(ctx)
	if err != nil || tok != "ya29.fresh-access" {
		t.Fatalf("refresh: %q %v", tok, err)
	}
	if r := q(t, f.one("POST", "/token").Body); r.Get("grant_type") != "refresh_token" || r.Get("refresh_token") != "1//keep-me" {
		t.Fatalf("refresh request: %v", r)
	}
	var st storedToken
	b, _ := store.ReadSecret("gw", tokenFile)
	_ = json.Unmarshal(b, &st)
	if st.Token.AccessToken != "ya29.fresh-access" {
		t.Fatalf("refreshed token not saved: %s", b)
	}
	if _, err := ts.Token(ctx); err != nil || len(f.find("POST", "/token")) != 1 {
		t.Fatalf("valid token should be reused: %v", err)
	}

	writeToken(t, store, expired)
	other := &auth.Account{Alias: "gw", Provider: Name, Mode: policy.ModeDraft, Username: "someone@tronai.example"}
	ts, _ = NewTokenSource(TokenSourceParams{Store: store, Account: other, Endpoint: ep, BaseURL: f.srv.URL})
	if _, err := ts.Token(ctx); errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "account lock") {
		t.Fatalf("lock on refresh: %v", err)
	}
}

func TestTokenRefreshInvalidGrant(t *testing.T) {
	f := newFakeGmail(t)
	f.on(route{method: "POST", path: "/token", status: 400, body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`})
	store := &auth.Store{Dir: t.TempDir()}
	writeToken(t, store, &oauth2.Token{AccessToken: "old", RefreshToken: "1//revoked", Expiry: time.Now().Add(-time.Hour)})
	acct := &auth.Account{Alias: "gw", Provider: Name, Mode: policy.ModeRead}
	ts, err := NewTokenSource(TokenSourceParams{Store: store, Account: acct, Endpoint: oauth2.Endpoint{TokenURL: f.srv.URL + "/token"}, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.Token(ctx)
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "--mode read") {
		t.Fatalf("invalid_grant: %v", err)
	}
	if _, err := NewTokenSource(TokenSourceParams{Store: &auth.Store{Dir: t.TempDir()}, Account: acct}); errs.ExitCode(err) != errs.ExitAuth {
		t.Fatalf("no stored login: %v", err)
	}
}

func TestLoadClientFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "installed.json")
	_ = os.WriteFile(good, []byte(`{"installed":{"client_id":"id-1","client_secret":"s-1","redirect_uris":["http://localhost"]}}`), 0o600)
	web := filepath.Join(dir, "web.json")
	_ = os.WriteFile(web, []byte(`{"web":{"client_id":"id-2","client_secret":"s-2"}}`), 0o600)
	c, err := LoadClientFile(good)
	if err != nil || c.ID != "id-1" || c.Secret != "s-1" {
		t.Fatalf("installed: %+v %v", c, err)
	}
	if _, err := LoadClientFile(web); errs.ExitCode(err) != errs.ExitUsage || !strings.Contains(err.Error(), "Desktop") {
		t.Fatalf("web client: %v", err)
	}
}

func TestScopesByMode(t *testing.T) {
	if s := Scopes(policy.ModeRead); len(s) != 1 || s[0] != scopeReadonly {
		t.Fatalf("read: %v", s)
	}
	for _, m := range []policy.Mode{policy.ModeDraft, policy.ModeSend} {
		if s := Scopes(m); len(s) != 1 || s[0] != scopeModify {
			t.Fatalf("%s: %v", m, s)
		}
	}
}
