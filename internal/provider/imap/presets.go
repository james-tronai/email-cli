package imap

import (
	"sort"
	"strings"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/errs"
)

// Preset is a provider's IMAP and SMTP endpoints. The TLS mode follows from
// the port: 993 and 465 are implicit TLS, 143, 587 and 25 use STARTTLS.
type Preset struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	IMAPHost string `json:"imap_host,omitempty"`
	IMAPPort int    `json:"imap_port,omitempty"`
	SMTPHost string `json:"smtp_host,omitempty"`
	SMTPPort int    `json:"smtp_port,omitempty"`
	// NeedsHost: the server is the user's own (--host sets IMAP and SMTP).
	NeedsHost bool `json:"needs_host,omitempty"`
	// Password is where the app password (or its local equivalent) comes from.
	Password string `json:"password"`
	// Domains pick the preset from the address when --preset is not given.
	Domains []string `json:"domains,omitempty"`
	// Refuse explains why this provider cannot be used with a password.
	Refuse string `json:"refuse,omitempty"`
}

// Presets, documented in docs/imap-presets.md. Keep both in step.
var Presets = map[string]Preset{
	"gmail": {Name: "gmail", Title: "Gmail (personal)",
		IMAPHost: GmailIMAPHost, IMAPPort: 993, SMTPHost: GmailSMTPHost, SMTPPort: 465,
		Password: "app password: https://myaccount.google.com/apppasswords (needs 2-Step Verification)",
		Domains:  []string{"gmail.com", "googlemail.com"}},
	"icloud": {Name: "icloud", Title: "iCloud Mail",
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, SMTPHost: "smtp.mail.me.com", SMTPPort: 587,
		Password: "app-specific password: https://account.apple.com → Sign-In and Security → App-Specific Passwords",
		Domains:  []string{"icloud.com", "me.com", "mac.com"}},
	"fastmail": {Name: "fastmail", Title: "Fastmail",
		IMAPHost: "imap.fastmail.com", IMAPPort: 993, SMTPHost: "smtp.fastmail.com", SMTPPort: 465,
		Password: "app password: Settings → Privacy & Security → Manage app passwords (not on the Basic plan)",
		Domains:  []string{"fastmail.com", "fastmail.fm"}},
	"aliyun": {Name: "aliyun", Title: "Alibaba Mail for business (阿里企业邮箱)",
		IMAPHost: "imap.qiye.aliyun.com", IMAPPort: 993, SMTPHost: "smtp.qiye.aliyun.com", SMTPPort: 465,
		Password: "三方客户端安全密码 (web mail → settings), if the organisation requires it; the admin may also have to allow third-party clients"},
	"qq": {Name: "qq", Title: "QQ Mail (QQ邮箱)",
		IMAPHost: "imap.qq.com", IMAPPort: 993, SMTPHost: "smtp.qq.com", SMTPPort: 465,
		Password: "授权码: web mail → 设置 → 账号 → 开启 IMAP/SMTP 服务",
		Domains:  []string{"qq.com", "foxmail.com"}},
	"163": {Name: "163", Title: "NetEase 163 Mail (网易163邮箱)",
		IMAPHost: "imap.163.com", IMAPPort: 993, SMTPHost: "smtp.163.com", SMTPPort: 465,
		Password: "授权码: web mail → 设置 → POP3/SMTP/IMAP → 开启 IMAP/SMTP 服务",
		Domains:  []string{"163.com"}},
	"mailu": {Name: "mailu", Title: "Mailu (self-hosted)",
		IMAPPort: 993, SMTPPort: 465, NeedsHost: true,
		Password: "the account password, or an authentication token from the Mailu web UI (Settings → Authentication tokens)"},
	"outlook": {Name: "outlook", Title: "Outlook.com / Hotmail / Live (personal)",
		Domains: []string{"outlook.com", "hotmail.com", "live.com", "msn.com"},
		Refuse: "personal Outlook.com accounts only accept OAuth for IMAP and SMTP since September 2024; app passwords no longer work. " +
			"Use a Microsoft 365 work or school account with --provider msgraph instead (personal Microsoft accounts are not supported)"},
}

// PresetNames lists the presets, sorted.
func PresetNames() []string {
	names := make([]string, 0, len(Presets))
	for n := range Presets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func presetForDomain(addr string) (Preset, bool) {
	d := domainOf(addr)
	for _, p := range Presets {
		for _, pd := range p.Domains {
			if d == pd {
				return p, true
			}
		}
	}
	return Preset{}, false
}

// ResolveServers turns --preset, --host and the explicit --imap-*/--smtp-*
// flags into the account's servers. Explicit flags win over the preset;
// without a preset, the address's domain may pick one (gmail.com → gmail).
func ResolveServers(username, preset, host string, s auth.Servers) (auth.Servers, error) {
	var p Preset
	switch {
	case preset != "":
		var ok bool
		if p, ok = Presets[strings.ToLower(preset)]; !ok {
			return s, errs.Usage("unknown --preset %q (want one of %s)", preset, strings.Join(PresetNames(), ", "))
		}
	case s.IMAPHost == "" && host == "":
		p, _ = presetForDomain(username)
	}
	if p.Refuse != "" {
		return s, errs.Usage("%s: %s", p.Title, p.Refuse)
	}
	if host != "" {
		if p.Name != "" && !p.NeedsHost {
			return s, errs.Usage("--host applies to presets for your own server (mailu); %s has fixed hosts", p.Name)
		}
		p.IMAPHost, p.SMTPHost = host, host
	} else if p.NeedsHost && s.IMAPHost == "" {
		return s, errs.Usage("--preset %s needs --host <your mail server>", p.Name)
	}
	if s.IMAPHost == "" {
		s.IMAPHost = p.IMAPHost
	}
	if s.IMAPPort == 0 {
		s.IMAPPort = p.IMAPPort
	}
	if s.SMTPHost == "" {
		s.SMTPHost = p.SMTPHost
	}
	if s.SMTPPort == 0 && strings.EqualFold(s.SMTPHost, p.SMTPHost) {
		s.SMTPPort = p.SMTPPort
	}
	if s.Preset == "" {
		s.Preset = p.Name
	}
	return DefaultServers(username, s)
}
