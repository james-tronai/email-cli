package mailmime

import (
	"strings"
	"testing"

	"github.com/james-tronai/email-cli/internal/provider"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func TestParseNested(t *testing.T) {
	m := Parse(crlf(`From: a@example.com
Subject: =?utf-8?b?w6l0w6k=?=
Content-Type: multipart/mixed; boundary=outer

--outer
Content-Type: multipart/alternative; boundary=inner

--inner
Content-Type: text/plain; charset=windows-1252
Content-Transfer-Encoding: quoted-printable

caf=E9
--inner
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: base64

PHA+Y2Fmw6k8L3A+
--inner--
--outer
Content-Type: image/png
Content-ID: <logo>
Content-Transfer-Encoding: base64

iVBORw==
--outer
Content-Type: message/rfc822

From: b@example.com
Subject: inner

hello
--outer
Content-Type: application/octet-stream
Content-Disposition: attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf

PDF
--outer--
`))
	if DecodeHeader(m.Header.Get("Subject")) != "été" {
		t.Fatalf("subject = %q", m.Header.Get("Subject"))
	}
	text, html := m.Bodies()
	if text != "café" || html != "<p>café</p>" {
		t.Fatalf("bodies = %q / %q", text, html)
	}
	atts := m.Attachments()
	want := []provider.AttachmentMeta{
		{ID: "2", Name: "part-2", Size: 4, MIME: "image/png", Inline: true},
		{ID: "3", Name: "message.eml", Size: int64(len("From: b@example.com\r\nSubject: inner\r\n\r\nhello")), MIME: "message/rfc822"},
		{ID: "4", Name: "résumé.pdf", Size: 3, MIME: "application/octet-stream"},
	}
	if len(atts) != len(want) {
		t.Fatalf("attachments = %+v", atts)
	}
	for i := range want {
		if atts[i] != want[i] {
			t.Errorf("attachment %d = %+v, want %+v", i, atts[i], want[i])
		}
	}
	if p := m.Find("1.2"); p == nil || p.MediaType != "text/html" {
		t.Fatalf("find 1.2 = %+v", p)
	}
	if files := m.Files(); len(files) != 2 || files[1].Name != "résumé.pdf" {
		t.Fatalf("files = %+v", files)
	}
}

func TestParseSinglePartAndDamage(t *testing.T) {
	m := Parse(crlf("Subject: x\n\nplain body\n"))
	if text, _ := m.Bodies(); text != "plain body\r\n" || m.Root.Path != "1" {
		t.Fatalf("single = %q path %q", text, m.Root.Path)
	}
	// A multipart without its closing boundary keeps what was read.
	m = Parse(crlf("Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nhi\n--b\nContent-Type: text/plain\n\ncut off"))
	if text, _ := m.Bodies(); text != "hi" {
		t.Fatalf("damaged = %q", text)
	}
}

func TestBuildRoundTrip(t *testing.T) {
	o := Outgoing{From: provider.Address{Email: "j@example.org"}, To: []provider.Address{{Name: "Ann", Email: "a@example.com"}},
		Subject: "Hi\r\nBcc: evil@x", Body: "line1\nline2 é", MessageID: "<id@example.org>",
		Attachments: []provider.AttachmentFile{{Name: "a.txt", MIME: "text/plain", Data: []byte("data")}}}
	b, err := o.Build(0, "")
	if err != nil {
		t.Fatal(err)
	}
	m := Parse(b)
	if m.Header.Get("Bcc") != "" || m.Header.Get("Message-Id") != "<id@example.org>" {
		t.Fatalf("header = %v", m.Header)
	}
	if text, _ := m.Bodies(); text != "line1\r\nline2 é" {
		t.Fatalf("body = %q", text)
	}
	if f := m.Files(); len(f) != 1 || string(f[0].Data) != "data" {
		t.Fatalf("files = %+v", f)
	}
	if _, err := o.Build(10, "Test"); err == nil {
		t.Fatal("size limit ignored")
	}
}
