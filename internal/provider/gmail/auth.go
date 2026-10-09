package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/auth/oauthflow"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

const (
	Name       = "gmail"
	tokenFile  = "google-token.json"
	clientFile = "google-client.json"

	scopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	scopeModify   = "https://www.googleapis.com/auth/gmail.modify"
)

// Google OAuth endpoints. Declared here instead of importing
// x/oauth2/google, which pulls in the GCE metadata client.
var Endpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// Scopes returns the scopes requested at login for a mode. Gmail has no
// scope that can create drafts without also being able to send
// (gmail.compose and gmail.modify both send), so draft and send share
// gmail.modify and only the policy gate stops a draft-mode account sending.
func Scopes(m policy.Mode) []string {
	if m >= policy.ModeDraft {
		return []string{scopeModify}
	}
	return []string{scopeReadonly}
}

// Client is the OAuth client (GCP "Desktop app" type). Google calls the
// secret of an installed-app client non-confidential, but it is still kept
// out of account.json and out of Git.
type Client struct {
	ID     string `json:"client_id"`
	Secret string `json:"client_secret"`
}

// LoadClientFile reads the JSON that the GCP console downloads for a
// Desktop OAuth client ({"installed": {"client_id": …, "client_secret": …}}).
func LoadClientFile(path string) (*Client, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Usage("--client-file: %v", err)
	}
	var f struct {
		Installed *Client `json:"installed"`
		Web       *Client `json:"web"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, errs.Usage("--client-file %s: %v", path, err)
	}
	if f.Installed == nil {
		if f.Web != nil {
			return nil, errs.Usage("--client-file %s is a Web application client; create a Desktop app client", path)
		}
		return nil, errs.Usage("--client-file %s has no \"installed\" client", path)
	}
	return f.Installed, nil
}

// SaveClient stores the OAuth client for the account (0600).
func SaveClient(store *auth.Store, alias string, c *Client) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return store.WriteSecret(alias, clientFile, b)
}

// LoadClient returns the stored OAuth client, or nil if there is none.
func LoadClient(store *auth.Store, alias string) (*Client, error) {
	b, err := store.ReadSecret(alias, clientFile)
	if err != nil || b == nil {
		return nil, err
	}
	var c Client
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", clientFile, err)
	}
	return &c, nil
}

// storedToken is google-token.json.
type storedToken struct {
	Token    *oauth2.Token `json:"token"`
	Scopes   []string      `json:"scopes"`
	IssuedAt time.Time     `json:"issued_at"` // when the refresh token was issued (login)
}

func oauthConfig(c *Client, endpoint oauth2.Endpoint, mode policy.Mode, redirect string) *oauth2.Config {
	return &oauth2.Config{ClientID: c.ID, ClientSecret: c.Secret, Endpoint: endpoint, Scopes: Scopes(mode), RedirectURL: redirect}
}

// ---- token source used by every command ----

type fileTokens struct {
	mu       sync.Mutex
	store    *auth.Store
	acct     *auth.Account
	cfg      *oauth2.Config
	st       *storedToken
	src      oauth2.TokenSource
	http     *http.Client
	baseURL  string
	verified bool
}

type TokenSourceParams struct {
	Store    *auth.Store
	Account  *auth.Account
	HTTP     *http.Client    // for token refresh and the identity check
	Endpoint oauth2.Endpoint // default Endpoint
	BaseURL  string          // Gmail base, for the identity check
}

// NewTokenSource returns the TokenSource for a logged-in gmail account.
// Refreshed tokens are written back to google-token.json; after each
// refresh the mailbox is checked against the account lock.
func NewTokenSource(p TokenSourceParams) (TokenSource, error) {
	relogin := fmt.Sprintf("run `email auth login -a %s --provider gmail --mode %s`", p.Account.Alias, p.Account.Mode)
	c, err := LoadClient(p.Store, p.Account.Alias)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errs.Auth("no OAuth client stored for %s; %s --client-file <client_secret.json>", p.Account.Alias, relogin)
	}
	b, err := p.Store.ReadSecret(p.Account.Alias, tokenFile)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, errs.Auth("no token for %s; %s", p.Account.Alias, relogin)
	}
	var st storedToken
	if err := json.Unmarshal(b, &st); err != nil || st.Token == nil {
		return nil, errs.Auth("%s is unreadable; %s", tokenFile, relogin)
	}
	ep := p.Endpoint
	if ep.TokenURL == "" {
		ep = Endpoint
	}
	return &fileTokens{store: p.Store, acct: p.Account, cfg: oauthConfig(c, ep, p.Account.Mode, ""), st: &st,
		http: p.HTTP, baseURL: firstNonEmpty(p.BaseURL, DefaultBaseURL)}, nil
}

func (f *fileTokens) Token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.st.Token.Valid() {
		// About to refresh: take the account's file lock (the watch
		// sidecar and the agent's commands share this token) and pick up
		// a token another process may have refreshed meanwhile.
		unlock, err := f.store.Lock(ctx, f.acct.Alias)
		if err != nil {
			if ctx.Err() != nil {
				return "", errs.Timeout(ctx.Err())
			}
			return "", fmt.Errorf("locking the token file: %w", err)
		}
		defer unlock()
		if b, err := f.store.ReadSecret(f.acct.Alias, tokenFile); err == nil && b != nil {
			var st storedToken
			if json.Unmarshal(b, &st) == nil && st.Token != nil && st.Token.AccessToken != f.st.Token.AccessToken {
				f.st, f.src = &st, nil
			}
		}
	}
	if f.src == nil {
		hctx := ctx
		if f.http != nil {
			hctx = context.WithValue(ctx, oauth2.HTTPClient, f.http)
		}
		// Detached from ctx cancellation only through the HTTP client; the
		// refresh itself is bounded by the caller's deadline.
		f.src = f.cfg.TokenSource(hctx, f.st.Token)
	}
	tok, err := f.src.Token()
	if err != nil {
		if ctx.Err() != nil {
			return "", errs.Timeout(ctx.Err())
		}
		return "", errs.Auth("token refresh failed (%s); run `email auth login -a %s --provider gmail --mode %s`",
			refreshErrorSummary(err), f.acct.Alias, f.acct.Mode)
	}
	if tok.AccessToken != f.st.Token.AccessToken {
		if !f.verified {
			addr, err := fetchProfileEmail(ctx, f.http, f.baseURL, tok.AccessToken)
			if err != nil {
				return "", err
			}
			if err := auth.CheckIdentity(f.acct, addr); err != nil {
				return "", err
			}
			f.verified = true
		}
		if tok.RefreshToken == "" {
			tok.RefreshToken = f.st.Token.RefreshToken
		}
		f.st.Token = tok
		b, err := json.Marshal(f.st)
		if err != nil {
			return "", err
		}
		if err := f.store.WriteSecret(f.acct.Alias, tokenFile, b); err != nil {
			return "", fmt.Errorf("saving refreshed token: %w", err)
		}
	}
	return tok.AccessToken, nil
}

func refreshErrorSummary(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode != "" {
		s := re.ErrorCode
		if re.ErrorDescription != "" {
			s += ": " + re.ErrorDescription
		}
		return s
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// fetchProfileEmail returns the mailbox address the token belongs to.
func fetchProfileEmail(ctx context.Context, h *http.Client, baseURL, accessToken string) (string, error) {
	cl, err := newClient(baseURL, h, staticToken(accessToken), "email-cli", "")
	if err != nil {
		return "", err
	}
	var prof struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := cl.doJSON(ctx, http.MethodGet, "/gmail/v1/users/me/profile", nil, &prof); err != nil {
		return "", err
	}
	if prof.EmailAddress == "" {
		return "", errs.Auth("Gmail profile returned no emailAddress")
	}
	return prof.EmailAddress, nil
}

// ---- login ----

type LoginParams struct {
	Store     *auth.Store
	Account   *auth.Account // Alias, Mode set; Username set on re-login (lock)
	Client    *Client
	HTTP      *http.Client
	Endpoint  oauth2.Endpoint // default Endpoint
	BaseURL   string          // Gmail base, for the profile call
	UserAgent string

	Manual      bool
	Stdin       io.Reader
	OnPrompt    func(oauthflow.Prompt) error
	OpenBrowser func(string) error
}

type LoginResult struct {
	Username string
	Scopes   []string
	Warnings []string
}

// authorizeURL builds Google's authorize URL. access_type=offline and
// prompt=consent make Google return a refresh token on every login;
// include_granted_scopes is left off so a draft login never inherits a
// broader earlier grant.
func authorizeURL(cfg *oauth2.Config, loginHint string) func(redirect, state, challenge string) string {
	return func(redirect, state, challenge string) string {
		c := *cfg
		c.RedirectURL = redirect
		opts := []oauth2.AuthCodeOption{
			oauth2.AccessTypeOffline,
			oauth2.SetAuthURLParam("prompt", "consent select_account"),
			oauth2.SetAuthURLParam("code_challenge", challenge),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		}
		if loginHint != "" {
			opts = append(opts, oauth2.SetAuthURLParam("login_hint", loginHint))
		}
		return strings.ReplaceAll(c.AuthCodeURL(state, opts...), "+", "%20")
	}
}

// Login runs the browser flow (loopback or --manual), redeems the code with
// the PKCE verifier, checks the granted scopes, locks the account to the
// Gmail profile address and writes google-token.json.
func Login(ctx context.Context, p LoginParams) (*LoginResult, error) {
	acct := p.Account
	if p.Client == nil || p.Client.ID == "" || p.Client.Secret == "" {
		return nil, errs.Usage("gmail needs an OAuth Desktop client: pass --client-file <client_secret.json> or --client-id and --client-secret (see docs/google-workspace-setup.md)")
	}
	ep := p.Endpoint
	if ep.TokenURL == "" {
		ep = Endpoint
	}
	cfg := oauthConfig(p.Client, ep, acct.Mode, "")
	r, err := oauthflow.Run(ctx, oauthflow.Params{
		AuthorizeURL: authorizeURL(cfg, acct.Username),
		Manual:       p.Manual,
		Stdin:        p.Stdin,
		OnPrompt:     p.OnPrompt,
		OpenBrowser:  p.OpenBrowser,
	})
	if err != nil {
		return nil, err
	}
	cfg.RedirectURL = r.RedirectURI
	hctx := ctx
	if p.HTTP != nil {
		hctx = context.WithValue(ctx, oauth2.HTTPClient, p.HTTP)
	}
	tok, err := cfg.Exchange(hctx, r.Code, oauth2.VerifierOption(r.CodeVerifier))
	if err != nil {
		if ctx.Err() != nil {
			return nil, errs.Timeout(ctx.Err())
		}
		return nil, errs.Auth("code redemption failed: %s", refreshErrorSummary(err))
	}
	if tok.RefreshToken == "" {
		return nil, errs.Auth("Google returned no refresh token; remove the app at https://myaccount.google.com/permissions and log in again")
	}
	granted := strings.Fields(fmt.Sprint(tok.Extra("scope")))
	for _, want := range Scopes(acct.Mode) {
		if !contains(granted, want) {
			return nil, errs.Auth("the consent screen did not grant %s (granted: %s); log in again and leave the Gmail permission ticked",
				want, strings.Join(granted, " "))
		}
	}

	addr, err := fetchProfileEmail(ctx, p.HTTP, firstNonEmpty(p.BaseURL, DefaultBaseURL), tok.AccessToken)
	if err != nil {
		return nil, err
	}
	if acct.Username != "" && !strings.EqualFold(acct.Username, addr) {
		return nil, errs.Auth("account lock: %s was locked to %s but %s logged in; use another alias or `email auth logout -a %s` first",
			acct.Alias, acct.Username, addr, acct.Alias)
	}

	out := &LoginResult{Username: addr, Scopes: granted}
	if acct.Mode < policy.ModeDraft && (contains(granted, scopeModify) || contains(granted, "https://mail.google.com/")) {
		out.Warnings = append(out.Warnings, "the token can modify mail although mode is read; revoke the app's access and log in again")
	}
	if acct.Mode >= policy.ModeDraft {
		out.Warnings = append(out.Warnings, "gmail.modify can also send mail; on Gmail only the policy file stops a draft-mode account from sending")
	}
	if d := domainOf(addr); d == "gmail.com" || d == "googlemail.com" {
		out.Warnings = append(out.Warnings, "personal Gmail account: an External OAuth app in Testing status expires refresh tokens after 7 days")
	}

	st := storedToken{Token: tok, Scopes: granted, IssuedAt: time.Now().UTC().Truncate(time.Second)}
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if err := SaveClient(p.Store, acct.Alias, p.Client); err != nil {
		return nil, err
	}
	if err := p.Store.WriteSecret(acct.Alias, tokenFile, b); err != nil {
		return nil, err
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func domainOf(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return ""
	}
	return strings.ToLower(addr[i+1:])
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// StaticToken is a TokenSource for tests.
func StaticToken(tok string) TokenSource { return staticToken(tok) }

// userPath is the base for all mailbox calls.
const userPath = "/gmail/v1/users/me"

func pathEsc(s string) string { return url.PathEscape(s) }
