package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingIsReadOnlyBuiltin(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != "builtin" || p.Default.Mode != ModeRead {
		t.Fatalf("got source=%s mode=%s", p.Source, p.Default.Mode)
	}
	if p.Signoff() != DefaultSignoff {
		t.Fatalf("builtin signoff should be on, got %q", p.Signoff())
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writePolicy(t, "[default]\nmode = \"draft\"\n[send]\nallowed_recipient_domain = [\"x.com\"]\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("want unknown keys error, got %v", err)
	}
}

func TestLoadRejectsBadMode(t *testing.T) {
	if _, err := Load(writePolicy(t, "[default]\nmode = \"admin\"\n")); err == nil {
		t.Fatal("want error")
	}
}

func TestLoadSignoffAndDomains(t *testing.T) {
	p, err := Load(writePolicy(t, `
[default]
mode = "draft"
signoff = ""
[send]
allowed_recipient_domains = ["@TronAI.com.au"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Signoff() != "" {
		t.Fatalf("explicit empty signoff should disable, got %q", p.Signoff())
	}
	if got := p.Send.AllowedRecipientDomains[0]; got != "tronai.com.au" {
		t.Fatalf("domain not normalised: %q", got)
	}
	// Unset fields keep builtin defaults.
	if !p.Send.BlockExternalForward || p.Send.MaxRecipients != 5 {
		t.Fatalf("defaults lost: %+v", p.Send)
	}
}

func TestCheckModes(t *testing.T) {
	cases := []struct {
		policy, account Mode
		op              Op
		allowed         bool
	}{
		{ModeDraft, ModeDraft, OpList, true},
		{ModeDraft, ModeDraft, OpDraftCreate, true},
		{ModeDraft, ModeDraft, OpSend, false},
		{ModeRead, ModeDraft, OpDraftCreate, false}, // policy caps the token
		{ModeSend, ModeRead, OpDraftCreate, false},  // token caps the policy
		{ModeSend, ModeDraft, OpSend, false},        // no Mail.Send scope
		{ModeRead, ModeRead, OpMark, false},
		{ModeDraft, ModeDraft, OpDraftDelete, true},
	}
	for _, c := range cases {
		p := Builtin()
		p.Default.Mode = c.policy
		d := p.Check(Request{Op: c.op, AccountMode: c.account, HasReplyCtx: true})
		if d.Allowed != c.allowed {
			t.Errorf("policy=%s account=%s op=%s: allowed=%v want %v (%s)", c.policy, c.account, c.op, d.Allowed, c.allowed, d.Reason)
		}
	}
}

func TestCheckTrashBlocked(t *testing.T) {
	p := Builtin()
	p.Default.Mode = ModeDraft
	if d := p.Check(Request{Op: OpMove, AccountMode: ModeDraft, Dest: "Trash"}); d.Allowed {
		t.Fatal("move to trash should be denied")
	}
	if d := p.Check(Request{Op: OpLabel, AccountMode: ModeDraft, AddLabels: []string{"trash"}}); d.Allowed {
		t.Fatal("label trash should be denied")
	}
	if d := p.Check(Request{Op: OpMove, AccountMode: ModeDraft, Dest: "archive"}); !d.Allowed {
		t.Fatalf("archive should be allowed: %s", d.Reason)
	}
	p.Delete.Enabled = true
	if d := p.Check(Request{Op: OpMove, AccountMode: ModeDraft, Dest: "trash"}); !d.Allowed {
		t.Fatalf("trash should be allowed when delete enabled: %s", d.Reason)
	}
}

func TestCheckForwardExternal(t *testing.T) {
	p := Builtin()
	p.Default.Mode = ModeDraft
	req := Request{Op: OpDraftForward, AccountMode: ModeDraft, AccountDomain: "tronai.com.au"}

	req.Recipients = []string{"a@tronai.com.au"}
	if d := p.Check(req); !d.Allowed {
		t.Fatalf("internal forward denied: %s", d.Reason)
	}
	req.Recipients = []string{"a@tronai.com.au", "evil@attacker.example"}
	if d := p.Check(req); d.Allowed || !strings.Contains(d.Reason, "evil@attacker.example") {
		t.Fatalf("external forward allowed: %+v", d)
	}
	// Subdomains are not the same domain.
	req.Recipients = []string{"a@tronai.com.au.attacker.example"}
	if d := p.Check(req); d.Allowed {
		t.Fatal("lookalike domain allowed")
	}
	p.Send.AllowedRecipientDomains = []string{"partner.com"}
	req.Recipients = []string{"x@partner.com"}
	if d := p.Check(req); !d.Allowed {
		t.Fatalf("allowlisted domain denied: %s", d.Reason)
	}
}

func TestCheckSendRules(t *testing.T) {
	p := Builtin()
	p.Default.Mode = ModeSend
	p.Send.AllowedRecipientDomains = []string{"tronai.com.au"}
	base := Request{Op: OpSend, AccountMode: ModeSend, HasReplyCtx: true, Recipients: []string{"a@tronai.com.au"}}

	if d := p.Check(base); !d.Allowed {
		t.Fatalf("denied: %s", d.Reason)
	}
	r := base
	r.HasReplyCtx = false
	if d := p.Check(r); d.Allowed {
		t.Fatal("new send without reply context allowed")
	}
	r = base
	r.Recipients = []string{"a@tronai.com.au", "b@gmail.com"}
	if d := p.Check(r); d.Allowed {
		t.Fatal("recipient outside allowlist allowed")
	}
	r = base
	r.Recipients = strings.Split("a@tronai.com.au,b@tronai.com.au,c@tronai.com.au,d@tronai.com.au,e@tronai.com.au,f@tronai.com.au", ",")
	if d := p.Check(r); d.Allowed {
		t.Fatal("too many recipients allowed")
	}
}

func TestCheckAPI(t *testing.T) {
	p := Builtin()
	p.Default.Mode = ModeDraft
	cases := []struct {
		method, path string
		account      Mode
		allowed      bool
	}{
		{"GET", "/me/mailFolders", ModeRead, true},
		{"POST", "/me/messages", ModeRead, false},
		{"POST", "/me/messages", ModeDraft, true},
		{"DELETE", "/me/messages/1", ModeDraft, false},
		{"POST", "/me/sendMail", ModeDraft, false},
		{"POST", "/me/messages/AAMk/send", ModeDraft, false},
		{"POST", "/me/messages/AAMk/replyAll", ModeDraft, false},
		{"POST", "/me/messages/AAMk/createReply", ModeDraft, true},
		{"POST", "/gmail/v1/users/me/drafts/send", ModeDraft, false},
		{"POST", "/gmail/v1/users/me/messages/send?uploadType=media", ModeDraft, false},
	}
	for _, c := range cases {
		d := p.Check(Request{Op: OpAPI, AccountMode: c.account, APIMethod: c.method, APIPath: c.path})
		if d.Allowed != c.allowed {
			t.Errorf("%s %s (account %s): allowed=%v want %v (%s)", c.method, c.path, c.account, d.Allowed, c.allowed, d.Reason)
		}
	}
}
