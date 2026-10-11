package imap

import (
	"strings"
	"testing"

	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/errs"
)

func TestResolveServers(t *testing.T) {
	for _, c := range []struct {
		name, user, preset, host string
		in                       auth.Servers
		want                     auth.Servers
	}{
		{"gmail by domain", "j@gmail.com", "", "", auth.Servers{},
			auth.Servers{IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 465, Preset: "gmail"}},
		{"icloud by domain, STARTTLS submission", "j@icloud.com", "", "", auth.Servers{},
			auth.Servers{IMAPHost: "imap.mail.me.com", IMAPPort: 993, SMTPHost: "smtp.mail.me.com", SMTPPort: 587, Preset: "icloud"}},
		{"aliyun preset for a company domain", "j@corp.example", "aliyun", "", auth.Servers{},
			auth.Servers{IMAPHost: "imap.qiye.aliyun.com", IMAPPort: 993, SMTPHost: "smtp.qiye.aliyun.com", SMTPPort: 465, Preset: "aliyun"}},
		{"163", "j@163.com", "", "", auth.Servers{},
			auth.Servers{IMAPHost: "imap.163.com", IMAPPort: 993, SMTPHost: "smtp.163.com", SMTPPort: 465, Preset: "163"}},
		{"mailu with --host", "j@tronai.example", "mailu", "mail.tronai.example", auth.Servers{},
			auth.Servers{IMAPHost: "mail.tronai.example", IMAPPort: 993, SMTPHost: "mail.tronai.example", SMTPPort: 465, Preset: "mailu"}},
		{"--host alone", "j@corp.example", "", "mx.corp.example", auth.Servers{},
			auth.Servers{IMAPHost: "mx.corp.example", IMAPPort: 993, SMTPHost: "mx.corp.example", SMTPPort: 465}},
		{"explicit flags win over the preset", "j@qq.com", "qq", "", auth.Servers{IMAPPort: 143, SMTPHost: "relay.example", SMTPPort: 587},
			auth.Servers{IMAPHost: "imap.qq.com", IMAPPort: 143, SMTPHost: "relay.example", SMTPPort: 587, Preset: "qq"}},
		{"explicit host: no domain preset", "j@gmail.com", "", "", auth.Servers{IMAPHost: "127.0.0.1", IMAPPort: 1},
			auth.Servers{IMAPHost: "127.0.0.1", IMAPPort: 1, SMTPPort: 465}},
	} {
		got, err := ResolveServers(c.user, c.preset, c.host, c.in)
		if err != nil || got != c.want {
			t.Errorf("%s:\n got %+v %v\nwant %+v", c.name, got, err, c.want)
		}
	}
}

func TestResolveServersRefusals(t *testing.T) {
	for _, c := range []struct{ user, preset, host, want string }{
		{"j@outlook.com", "", "", "OAuth"},
		{"j@hotmail.com", "", "", "msgraph"},
		{"j@corp.example", "outlook", "", "September 2024"},
		{"j@corp.example", "mailu", "", "needs --host"},
		{"j@corp.example", "gmail", "x.example", "fixed hosts"},
		{"j@corp.example", "yahoo", "", "unknown --preset"},
		{"j@corp.example", "", "", "--imap-host is required"},
	} {
		_, err := ResolveServers(c.user, c.preset, c.host, auth.Servers{})
		if err == nil || errs.ExitCode(err) != errs.ExitUsage || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s %s: %v", c.user, c.preset, c.host, err)
		}
	}
}

func TestPresetsAreComplete(t *testing.T) {
	for name, p := range Presets {
		if p.Name != name || p.Title == "" {
			t.Errorf("%s: name/title", name)
		}
		if p.Refuse != "" {
			continue
		}
		if p.Password == "" || p.IMAPPort == 0 || p.SMTPPort == 0 || (!p.NeedsHost && (p.IMAPHost == "" || p.SMTPHost == "")) {
			t.Errorf("%s incomplete: %+v", name, p)
		}
	}
}
