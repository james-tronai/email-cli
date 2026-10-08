package msgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/auth/oauthflow"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

const (
	Name          = "msgraph"
	DefaultTenant = "organizations" // work/school accounts only; never "common"
	cacheFile     = "msal.json"
)

// Scopes returns the delegated scopes requested at login for a mode.
func Scopes(m policy.Mode) []string {
	s := []string{"User.Read", "Mail.Read", "offline_access"}
	if m >= policy.ModeDraft {
		s = append(s, "Mail.ReadWrite")
	}
	if m >= policy.ModeSend {
		s = append(s, "Mail.Send")
	}
	return s
}

// tokenScopes drops offline_access, which MSAL adds itself and rejects in
// silent requests.
func tokenScopes(m policy.Mode) []string {
	var out []string
	for _, s := range Scopes(m) {
		if s != "offline_access" {
			out = append(out, s)
		}
	}
	return out
}

func authority(tenant string) string {
	if tenant == "" {
		tenant = DefaultTenant
	}
	return "https://login.microsoftonline.com/" + tenant
}

// fileCache persists the MSAL cache to accounts/<alias>/msal.json (0600).
type fileCache struct {
	store  *auth.Store
	alias  string
	homeID string // when set, entries for other accounts are dropped on load
}

func (f *fileCache) Replace(_ context.Context, u cache.Unmarshaler, _ cache.ReplaceHints) error {
	b, err := f.store.ReadSecret(f.alias, cacheFile)
	if err != nil || b == nil {
		return err
	}
	b, err = dedupeCache(b, f.homeID)
	if err != nil {
		return err
	}
	return u.Unmarshal(b)
}

func (f *fileCache) Export(_ context.Context, m cache.Marshaler, _ cache.ExportHints) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	return f.store.WriteSecret(f.alias, cacheFile, b)
}

// dedupeCache cleans a serialized MSAL cache before MSAL loads it:
//   - with homeID set, drops every entry (tokens, accounts) belonging to
//     another account, so silent auth can never pick the wrong mailbox;
//   - keeps at most one refresh token per (account, environment, client),
//     preferring the family refresh token MSAL itself prefers.
//
// Duplicate refresh tokens make MSAL pick an arbitrary (possibly revoked)
// one; Softeria's ms-365-mcp-server hit exactly this.
func dedupeCache(b []byte, homeID string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("msal cache: %w", err)
	}
	type entry struct {
		HomeAccountID string `json:"home_account_id"`
		Environment   string `json:"environment"`
		ClientID      string `json:"client_id"`
		FamilyID      string `json:"family_id"`
	}
	for section, raw := range top {
		var items map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			continue // not a map section (unknown future field): leave alone
		}
		parsed := map[string]entry{}
		for k, v := range items {
			var e entry
			_ = json.Unmarshal(v, &e)
			if homeID != "" && e.HomeAccountID != "" && !strings.EqualFold(e.HomeAccountID, homeID) {
				delete(items, k)
				continue
			}
			parsed[k] = e
		}
		if section == "RefreshToken" {
			groups := map[string][]string{}
			for k, e := range parsed {
				g := strings.ToLower(e.HomeAccountID + "|" + e.Environment + "|" + e.ClientID)
				groups[g] = append(groups[g], k)
			}
			for _, keys := range groups {
				if len(keys) < 2 {
					continue
				}
				sort.Slice(keys, func(i, j int) bool {
					fi, fj := parsed[keys[i]].FamilyID != "", parsed[keys[j]].FamilyID != ""
					if fi != fj {
						return fi
					}
					return keys[i] < keys[j]
				})
				for _, k := range keys[1:] {
					delete(items, k)
				}
			}
		}
		nb, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		top[section] = nb
	}
	return json.Marshal(top)
}

// msalClient is the subset of public.Client used for silent auth (faked in tests).
type msalClient interface {
	Accounts(ctx context.Context) ([]public.Account, error)
	AcquireTokenSilent(ctx context.Context, scopes []string, opts ...public.AcquireSilentOption) (public.AuthResult, error)
}

func newMSAL(store *auth.Store, acct *auth.Account, h *http.Client) (public.Client, error) {
	if acct.ClientID == "" {
		return public.Client{}, errs.Usage("account %s has no client id; log in again with --client-id", acct.Alias)
	}
	opts := []public.Option{
		public.WithAuthority(authority(acct.TenantID)),
		public.WithCache(&fileCache{store: store, alias: acct.Alias, homeID: acct.HomeAccountID}),
	}
	if h != nil {
		opts = append(opts, public.WithHTTPClient(h))
	}
	c, err := public.New(acct.ClientID, opts...)
	if err != nil {
		return public.Client{}, errs.Usage("msal: %v", err)
	}
	return c, nil
}

// silentTokens acquires tokens from the cache/refresh token only, and
// enforces the account lock on every call.
type silentTokens struct {
	msal  msalClient
	acct  *auth.Account
	token string
	exp   time.Time
}

// NewTokenSource returns a TokenSource for a logged-in msgraph account.
func NewTokenSource(store *auth.Store, acct *auth.Account, h *http.Client) (TokenSource, error) {
	c, err := newMSAL(store, acct, h)
	if err != nil {
		return nil, err
	}
	return &silentTokens{msal: c, acct: acct}, nil
}

func (s *silentTokens) Token(ctx context.Context) (string, error) {
	if s.token != "" && time.Until(s.exp) > time.Minute {
		return s.token, nil
	}
	relogin := fmt.Sprintf("run `email auth login -a %s --provider msgraph --mode %s`", s.acct.Alias, s.acct.Mode)
	accts, err := s.msal.Accounts(ctx)
	if err != nil {
		return "", errs.Auth("reading token cache: %v; %s", err, relogin)
	}
	var match *public.Account
	for i := range accts {
		if s.acct.HomeAccountID == "" || strings.EqualFold(accts[i].HomeAccountID, s.acct.HomeAccountID) {
			match = &accts[i]
			break
		}
	}
	if match == nil {
		return "", errs.Auth("no cached login for %s; %s", s.acct.Alias, relogin)
	}
	if err := auth.CheckIdentity(s.acct, match.PreferredUsername); err != nil {
		return "", err
	}
	res, err := s.msal.AcquireTokenSilent(ctx, tokenScopes(s.acct.Mode), public.WithSilentAccount(*match))
	if err != nil {
		if ctx.Err() != nil {
			return "", errs.Timeout(ctx.Err())
		}
		return "", errs.Auth("token refresh failed (%s); %s", authErrorSummary(err), relogin)
	}
	s.token, s.exp = res.AccessToken, res.ExpiresOn
	return s.token, nil
}

// authErrorSummary extracts the AAD error code (invalid_grant,
// interaction_required, AADSTS…) without dumping the whole response.
func authErrorSummary(err error) string {
	msg := err.Error()
	for _, k := range []string{"interaction_required", "invalid_grant", "consent_required", "login_required"} {
		if strings.Contains(msg, k) {
			code := k
			if i := strings.Index(msg, "AADSTS"); i >= 0 {
				end := i + 6
				for end < len(msg) && msg[end] >= '0' && msg[end] <= '9' {
					end++
				}
				code += " " + msg[i:end]
			}
			return code
		}
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// DeviceCodeInfo is what the human needs to complete login.
type DeviceCodeInfo struct {
	VerificationURI string `json:"verification_uri"`
	UserCode        string `json:"user_code"`
	ExpiresIn       int    `json:"expires_in"`
	Message         string `json:"message"`
}

// Login flows.
const (
	FlowAuthCode = "auth-code" // authorization code + PKCE (default)
	FlowDevice   = "device"    // device code; blocked by Entra Security Defaults
)

type LoginParams struct {
	Store     *auth.Store
	Account   *auth.Account // Alias, Mode, TenantID, ClientID set
	HTTP      *http.Client
	BaseURL   string // Graph base, for /me
	UserAgent string

	Flow string // FlowAuthCode (default) or FlowDevice

	// Auth code flow.
	Manual      bool      // print the URL, read the pasted redirect URL from Stdin
	Stdin       io.Reader // for Manual
	OnPrompt    func(oauthflow.Prompt) error
	OpenBrowser func(string) error // loopback only

	// Device code flow: called once the code is available, before blocking.
	OnDeviceCode func(DeviceCodeInfo) error
}

type LoginResult struct {
	Username      string
	HomeAccountID string
	Granted       []string
	Warnings      []string
}

// authorizeURL is the Entra v2 authorize endpoint for the auth code flow.
func authorizeURL(acct *auth.Account) func(redirect, state, challenge string) string {
	return func(redirect, state, challenge string) string {
		q := url.Values{}
		q.Set("client_id", acct.ClientID)
		q.Set("response_type", "code")
		q.Set("response_mode", "query")
		q.Set("redirect_uri", redirect)
		q.Set("scope", "openid profile "+strings.Join(Scopes(acct.Mode), " "))
		q.Set("state", state)
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		q.Set("prompt", "select_account")
		if acct.Username != "" {
			q.Set("login_hint", acct.Username)
		}
		return authority(acct.TenantID) + "/oauth2/v2.0/authorize?" + strings.ReplaceAll(q.Encode(), "+", "%20")
	}
}

// Login signs the user in (auth code + PKCE by default, device code on
// request), then locks the account to the mailbox returned by /me. The
// previous token cache for the alias is discarded.
func Login(ctx context.Context, p LoginParams) (*LoginResult, error) {
	acct := p.Account
	if acct.ClientID == "" {
		return nil, errs.Usage("--client-id is required for msgraph (see docs/microsoft-365-setup.md)")
	}
	if strings.EqualFold(acct.TenantID, "common") || strings.EqualFold(acct.TenantID, "consumers") {
		return nil, errs.Usage("--tenant %s is not allowed: only work/school accounts are supported", acct.TenantID)
	}
	if acct.TenantID == "" {
		acct.TenantID = DefaultTenant
	}
	flow := p.Flow
	if flow == "" {
		flow = FlowAuthCode
	}
	if flow != FlowAuthCode && flow != FlowDevice {
		return nil, errs.Usage("--flow must be %s or %s", FlowAuthCode, FlowDevice)
	}
	if p.Manual && flow == FlowDevice {
		return nil, errs.Usage("--manual applies to --flow %s only", FlowAuthCode)
	}
	if err := p.Store.WriteSecret(acct.Alias, cacheFile, []byte("{}")); err != nil {
		return nil, err
	}
	acct.HomeAccountID = "" // fresh login: no filtering until we know who logged in
	c, err := newMSAL(p.Store, acct, p.HTTP)
	if err != nil {
		return nil, err
	}

	var res public.AuthResult
	if flow == FlowDevice {
		res, err = deviceCodeLogin(ctx, c, acct, p.OnDeviceCode)
	} else {
		res, err = authCodeLogin(ctx, c, acct, p)
	}
	if err != nil {
		return nil, err
	}
	return identify(ctx, p, res)
}

func authCodeLogin(ctx context.Context, c public.Client, acct *auth.Account, p LoginParams) (public.AuthResult, error) {
	r, err := oauthflow.Run(ctx, oauthflow.Params{
		AuthorizeURL: authorizeURL(acct),
		Manual:       p.Manual,
		Stdin:        p.Stdin,
		OnPrompt:     p.OnPrompt,
		OpenBrowser:  p.OpenBrowser,
	})
	if err != nil {
		return public.AuthResult{}, err
	}
	res, err := c.AcquireTokenByAuthCode(ctx, r.Code, r.RedirectURI, tokenScopes(acct.Mode), public.WithChallenge(r.CodeVerifier))
	if err != nil {
		return public.AuthResult{}, errs.Auth("code redemption failed: %s", authErrorSummary(err))
	}
	return res, nil
}

func deviceCodeLogin(ctx context.Context, c public.Client, acct *auth.Account, onCode func(DeviceCodeInfo) error) (public.AuthResult, error) {
	dc, err := c.AcquireTokenByDeviceCode(ctx, Scopes(acct.Mode))
	if err != nil {
		return public.AuthResult{}, errs.Auth("device code request failed: %s", authErrorSummary(err))
	}
	info := DeviceCodeInfo{
		VerificationURI: dc.Result.VerificationURL,
		UserCode:        dc.Result.UserCode,
		ExpiresIn:       int(time.Until(dc.Result.ExpiresOn).Round(time.Second).Seconds()),
		Message:         dc.Result.Message,
	}
	if onCode != nil {
		if err := onCode(info); err != nil {
			return public.AuthResult{}, err
		}
	}
	res, err := dc.AuthenticationResult(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return public.AuthResult{}, errs.Auth("device code login timed out; run login again")
		}
		return public.AuthResult{}, errs.Auth("device code login failed: %s (Entra Security Defaults block device code; use the default --flow %s)",
			authErrorSummary(err), FlowAuthCode)
	}
	return res, nil
}

// identify checks the fresh token against /me and returns the locked identity.
func identify(ctx context.Context, p LoginParams, res public.AuthResult) (*LoginResult, error) {
	acct := p.Account
	// Identify the mailbox with the fresh token.
	cl, err := newClient(firstNonEmpty(p.BaseURL, DefaultBaseURL), p.HTTP, staticToken(res.AccessToken), p.UserAgent, acct.Alias)
	if err != nil {
		return nil, err
	}
	var me struct {
		UserPrincipalName string `json:"userPrincipalName"`
		Mail              string `json:"mail"`
	}
	if err := cl.doJSON(ctx, http.MethodGet, "/me?$select=userPrincipalName,mail", nil, &me, nil); err != nil {
		return nil, err
	}
	username := firstNonEmpty(me.UserPrincipalName, me.Mail)
	if username == "" {
		return nil, errs.Auth("/me returned no userPrincipalName or mail")
	}
	if acct.Username != "" && !strings.EqualFold(acct.Username, username) &&
		!strings.EqualFold(acct.Username, me.Mail) {
		return nil, errs.Auth("account lock: %s was locked to %s but %s logged in; use another alias or `email auth logout -a %s` first",
			acct.Alias, acct.Username, username, acct.Alias)
	}
	if !strings.EqualFold(res.Account.PreferredUsername, username) && !strings.EqualFold(res.Account.PreferredUsername, me.Mail) {
		return nil, errs.Auth("token account %s does not match /me %s", res.Account.PreferredUsername, username)
	}

	out := &LoginResult{Username: username, HomeAccountID: res.Account.HomeAccountID, Granted: res.GrantedScopes}
	if acct.Mode < policy.ModeSend && hasScope(res.GrantedScopes, "Mail.Send") {
		out.Warnings = append(out.Warnings, "the token carries Mail.Send although mode is "+acct.Mode.String()+
			": the app registration has Mail.Send consented. Remove it from API permissions so draft mode cannot send outside the CLI.")
	}
	return out, nil
}

func hasScope(granted []string, want string) bool {
	for _, g := range granted {
		// Granted scopes may be fully qualified (https://graph.microsoft.com/Mail.Send).
		if strings.EqualFold(g, want) || strings.HasSuffix(strings.ToLower(g), "/"+strings.ToLower(want)) {
			return true
		}
	}
	return false
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// StaticToken is a TokenSource for tests.
func StaticToken(tok string) TokenSource { return staticToken(tok) }
