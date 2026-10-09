// Package policy is the gate every operation passes through.
//
// The policy file is written by the deployer and is read-only to the agent
// (in a Pod it is a ConfigMap mount). The effective mode for an operation is
// the lower of the policy mode and the mode the account logged in with, so a
// policy can never grant more than the token's scopes and a token can never
// be used beyond what the policy allows.
package policy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Mode int

const (
	ModeNone Mode = iota
	ModeRead
	ModeDraft
	ModeSend
)

func (m Mode) String() string {
	switch m {
	case ModeRead:
		return "read"
	case ModeDraft:
		return "draft"
	case ModeSend:
		return "send"
	}
	return "none"
}

func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read":
		return ModeRead, nil
	case "draft":
		return ModeDraft, nil
	case "send":
		return ModeSend, nil
	}
	return ModeNone, fmt.Errorf("invalid mode %q (want read, draft or send)", s)
}

func (m Mode) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

func (m *Mode) UnmarshalText(b []byte) error {
	v, err := ParseMode(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// DefaultSignoff is appended to agent-written bodies unless the policy sets
// `signoff` explicitly (an empty string disables it).
const DefaultSignoff = "\n\n-- \nSent by an automated assistant"

type Policy struct {
	Default DefaultSection `toml:"default" json:"default"`
	Send    SendSection    `toml:"send" json:"send"`
	Delete  DeleteSection  `toml:"delete" json:"delete"`
	Watch   WatchSection   `toml:"watch" json:"watch"`

	Source string `toml:"-" json:"source"` // file path, or "builtin"
}

type DefaultSection struct {
	Mode    Mode    `toml:"mode" json:"mode"`
	Signoff *string `toml:"signoff" json:"signoff"`
}

type SendSection struct {
	AllowedRecipientDomains []string `toml:"allowed_recipient_domains" json:"allowed_recipient_domains"`
	MaxRecipients           int      `toml:"max_recipients" json:"max_recipients"` // 0 = unlimited
	BlockExternalForward    bool     `toml:"block_external_forward" json:"block_external_forward"`
	RequireReplyContext     bool     `toml:"require_reply_context" json:"require_reply_context"`
}

type DeleteSection struct {
	Enabled bool `toml:"enabled" json:"enabled"`
}

// WatchSection filters what `email watch` notifies about. Entries match
// as case-insensitive substrings: "noreply@" or a "@domain" suffix.
type WatchSection struct {
	IgnoreSenders  []string `toml:"ignore_senders" json:"ignore_senders"`
	IgnoreSubjects []string `toml:"ignore_subjects" json:"ignore_subjects"`
	// OnlyUnread skips mail already read (on the phone, say) by the time
	// watch sees it.
	OnlyUnread bool `toml:"only_unread" json:"only_unread"`
}

// Builtin is used when no policy file exists: read-only, which is the safe
// failure mode for a missing or mis-mounted ConfigMap.
func Builtin() *Policy {
	return &Policy{
		Default: DefaultSection{Mode: ModeRead},
		Send: SendSection{
			MaxRecipients:        5,
			BlockExternalForward: true,
			RequireReplyContext:  true,
		},
		Watch:  WatchSection{OnlyUnread: true},
		Source: "builtin",
	}
}

// Signoff returns the text to append to agent-written bodies.
func (p *Policy) Signoff() string {
	if p.Default.Signoff == nil {
		return DefaultSignoff
	}
	return *p.Default.Signoff
}

// DefaultPath resolves the policy file: $EMAIL_POLICY_FILE, else
// <configDir>/policy.toml.
func DefaultPath(configDir string) string {
	if p := os.Getenv("EMAIL_POLICY_FILE"); p != "" {
		return p
	}
	return filepath.Join(configDir, "policy.toml")
}

// Load reads the policy at path. A missing file yields Builtin(); a file that
// exists but fails to parse, or has unknown keys, is an error — a typo in a
// security config must not silently fall back to defaults.
func Load(path string) (*Policy, error) {
	p := Builtin()
	md, err := toml.DecodeFile(path, p)
	if errors.Is(err, fs.ErrNotExist) {
		return Builtin(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("policy %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if p.Default.Mode == ModeNone {
		return nil, fmt.Errorf("policy %s: [default] mode is required", path)
	}
	for i, d := range p.Send.AllowedRecipientDomains {
		p.Send.AllowedRecipientDomains[i] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
	}
	for _, l := range []*[]string{&p.Watch.IgnoreSenders, &p.Watch.IgnoreSubjects} {
		for i, v := range *l {
			v = strings.ToLower(strings.TrimSpace(v))
			if v == "" {
				// An empty substring matches every message.
				return nil, fmt.Errorf("policy %s: [watch] has an empty ignore entry", path)
			}
			(*l)[i] = v
		}
	}
	p.Source = path
	return p, nil
}
