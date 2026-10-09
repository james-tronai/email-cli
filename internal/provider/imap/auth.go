package imap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"strings"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

// credentialsFile holds the password, 0600, next to account.json. It is
// never printed: account.json (what auth status shows) has only the hosts.
const credentialsFile = "imap-credentials.json"

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func isGmailDomain(addr string) bool {
	d := domainOf(addr)
	return d == "gmail.com" || d == "googlemail.com"
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// DefaultServers fills in what the user left out: Gmail's hosts for a
// gmail.com address, the standard ports everywhere.
func DefaultServers(username string, s auth.Servers) (auth.Servers, error) {
	if s.IMAPHost == "" && isGmailDomain(username) {
		s.IMAPHost = GmailIMAPHost
	}
	if strings.EqualFold(s.IMAPHost, GmailIMAPHost) && s.SMTPHost == "" {
		s.SMTPHost = GmailSMTPHost
	}
	if s.IMAPHost == "" {
		return s, errs.Usage("--imap-host is required for %s (only gmail.com addresses have defaults)", username)
	}
	if s.IMAPPort == 0 {
		s.IMAPPort = DefaultIMAPPort
	}
	if s.SMTPPort == 0 {
		s.SMTPPort = DefaultSMTPPort
	}
	return s, nil
}

type LoginParams struct {
	Store    *auth.Store
	Account  *auth.Account // Alias, Mode, Username (the address), Servers set
	Password string
	TLS      *tls.Config
}

type LoginResult struct {
	Username string
	Gmail    bool // the server announced X-GM-EXT-1
	Warnings []string
}

// Login checks the password with one IMAP LOGIN, then stores it. The
// alias stays locked to the address it was first logged in with.
func Login(ctx context.Context, p LoginParams) (*LoginResult, error) {
	acct := p.Account
	addr := strings.TrimSpace(acct.Username)
	if !strings.Contains(addr, "@") {
		return nil, errs.Usage("--username must be the mailbox address (user@example.com)")
	}
	if p.Password == "" {
		return nil, errs.Usage("no password on stdin")
	}
	if prev, err := p.Store.LoadAccount(acct.Alias); err == nil {
		if prev.Provider != Name {
			return nil, errs.Usage("account %s uses provider %s; log it out first", acct.Alias, prev.Provider)
		}
		if !strings.EqualFold(prev.Username, addr) {
			return nil, errs.Auth("account lock: %s was locked to %s but %s logged in; use another alias or `email auth logout -a %s` first",
				acct.Alias, prev.Username, addr, acct.Alias)
		}
	}
	if acct.Servers == nil {
		acct.Servers = &auth.Servers{}
	}
	s, err := DefaultServers(addr, *acct.Servers)
	if err != nil {
		return nil, err
	}
	acct.Servers = &s

	c, err := dial(ctx, s.IMAPHost, s.IMAPPort, p.TLS)
	if err != nil {
		return nil, err
	}
	defer c.close()
	if err := login(ctx, c, addr, p.Password); err != nil {
		return nil, err
	}
	res := &LoginResult{Username: addr, Gmail: c.has("X-GM-EXT-1")}

	b, err := json.Marshal(credentials{Username: addr, Password: p.Password})
	if err != nil {
		return nil, err
	}
	if err := p.Store.WriteSecret(acct.Alias, credentialsFile, b); err != nil {
		return nil, err
	}
	if acct.Mode < policy.ModeSend {
		res.Warnings = append(res.Warnings, "an app password can also change and send mail; for an imap account only the policy file and this client keep "+
			acct.Mode.String()+" mode (it never connects to SMTP below send mode)")
	}
	if isGmailDomain(addr) {
		res.Warnings = append(res.Warnings, "personal Gmail account: deleting the app password at https://myaccount.google.com/apppasswords revokes this login, as does changing the Google password")
	}
	return res, nil
}

// LoadCredentials reads the stored password and enforces the account lock.
func LoadCredentials(store *auth.Store, acct *auth.Account) (string, error) {
	b, err := store.ReadSecret(acct.Alias, credentialsFile)
	if err != nil {
		return "", err
	}
	if b == nil {
		return "", errs.Auth("no stored password for %s; run `email auth login -a %s --provider imap --username %s --password-stdin`",
			acct.Alias, acct.Alias, acct.Username)
	}
	var c credentials
	if err := json.Unmarshal(b, &c); err != nil || c.Password == "" {
		return "", errs.Auth("%s is damaged; log in again", credentialsFile)
	}
	if err := auth.CheckIdentity(acct, c.Username); err != nil {
		return "", err
	}
	return c.Password, nil
}
