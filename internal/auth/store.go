// Package auth stores account metadata and provider tokens, and enforces the
// account lock (the mailbox a token belongs to must match the one recorded at
// login).
//
// Layout under the config dir:
//
//	config.toml                     default_account
//	accounts/<alias>/account.json   Account metadata
//	accounts/<alias>/<secret>       provider token caches (0600)
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
)

type Account struct {
	Alias    string      `json:"alias"`
	Provider string      `json:"provider"`
	Mode     policy.Mode `json:"mode"`
	Username string      `json:"username"` // locked identity (UPN / email)
	TenantID string      `json:"tenant_id,omitempty"`
	ClientID string      `json:"client_id,omitempty"`
	// HomeAccountID is the provider's stable account id (MSAL home_account_id).
	HomeAccountID string   `json:"home_account_id,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
	// Servers are the hosts of an imap account (the password is stored
	// separately, never in account.json).
	Servers   *Servers  `json:"servers,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Servers are an imap account's IMAP and SMTP endpoints.
type Servers struct {
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	SMTPHost string `json:"smtp_host,omitempty"`
	SMTPPort int    `json:"smtp_port,omitempty"`
}

// Domain is the domain of the locked mailbox address.
func (a *Account) Domain() string {
	i := strings.LastIndexByte(a.Username, '@')
	if i < 0 {
		return ""
	}
	return strings.ToLower(a.Username[i+1:])
}

type Config struct {
	DefaultAccount string `toml:"default_account"`
}

type Store struct {
	Dir string
}

// DefaultConfigDir is $EMAIL_CONFIG_DIR, else $XDG_CONFIG_HOME/email, else
// ~/.config/email (also on macOS: one layout everywhere).
func DefaultConfigDir() string {
	if d := os.Getenv("EMAIL_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "email")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "email")
}

var aliasRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func ValidateAlias(alias string) error {
	if !aliasRE.MatchString(alias) {
		return errs.Usage("invalid account alias %q (lowercase letters, digits, . _ -; max 64)", alias)
	}
	return nil
}

func (s *Store) accountsDir() string { return filepath.Join(s.Dir, "accounts") }

func (s *Store) AccountDir(alias string) string { return filepath.Join(s.accountsDir(), alias) }

func (s *Store) LoadConfig() (*Config, error) {
	var c Config
	_, err := toml.DecodeFile(filepath.Join(s.Dir, "config.toml"), &c)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config.toml: %w", err)
	}
	return &c, nil
}

func (s *Store) SaveConfig(c *Config) error {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.Dir, "config.toml"), []byte(b.String()))
}

func (s *Store) SaveAccount(a *Account) error {
	if err := ValidateAlias(a.Alias); err != nil {
		return err
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.AccountDir(a.Alias), "account.json"), append(b, '\n'))
}

func (s *Store) LoadAccount(alias string) (*Account, error) {
	if err := ValidateAlias(alias); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(s.AccountDir(alias), "account.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.Auth("account %q not found; run `email auth login -a %s --provider <msgraph|gmail|imap>`", alias, alias)
	}
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("account %s: %w", alias, err)
	}
	return &a, nil
}

func (s *Store) ListAccounts() ([]Account, error) {
	ents, err := os.ReadDir(s.accountsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Account
	for _, e := range ents {
		if !e.IsDir() || ValidateAlias(e.Name()) != nil {
			continue
		}
		a, err := s.LoadAccount(e.Name())
		if err != nil {
			continue // half-written login; ignore
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

func (s *Store) DeleteAccount(alias string) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	return os.RemoveAll(s.AccountDir(alias))
}

// ResolveAlias picks the account to use: explicit flag, else $EMAIL_ACCOUNT,
// else config default_account, else the only account if there is exactly one.
func (s *Store) ResolveAlias(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv("EMAIL_ACCOUNT"); env != "" {
		return env, nil
	}
	c, err := s.LoadConfig()
	if err != nil {
		return "", err
	}
	if c.DefaultAccount != "" {
		return c.DefaultAccount, nil
	}
	accts, err := s.ListAccounts()
	if err != nil {
		return "", err
	}
	switch len(accts) {
	case 0:
		return "", errs.Auth("no accounts configured; run `email auth login`")
	case 1:
		return accts[0].Alias, nil
	}
	return "", errs.Usage("several accounts configured; pass -a <alias> or set default_account")
}

// WriteSecret writes a token cache file for the account (0600, atomic).
func (s *Store) WriteSecret(alias, name string, data []byte) error {
	return writeFileAtomic(filepath.Join(s.AccountDir(alias), name), data)
}

// ReadSecret returns (nil, nil) when the secret does not exist.
func (s *Store) ReadSecret(alias, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.AccountDir(alias), name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// CheckIdentity enforces the account lock.
func CheckIdentity(a *Account, actual string) error {
	if a.Username == "" || strings.EqualFold(a.Username, actual) {
		return nil
	}
	return errs.Auth("account lock: %q is locked to %s but the token belongs to %s; run `email auth login -a %s` again",
		a.Alias, a.Username, actual, a.Alias)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
