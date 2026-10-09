package imap

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

func readAll(t *testing.T, wire string) []*response {
	t.Helper()
	a, b := net.Pipe()
	go func() { b.Write([]byte(wire)); b.Close() }()
	c := &conn{nc: a, r: bufio.NewReader(a), w: bufio.NewWriter(a)}
	_ = a.SetDeadline(time.Now().Add(2 * time.Second))
	var out []*response
	for {
		r, err := c.readResponse()
		if err != nil {
			return out
		}
		out = append(out, r)
	}
}

func TestParseResponses(t *testing.T) {
	rs := readAll(t, "* 3 FETCH (UID 17 X-GM-LABELS (\"\\\\Inbox\" Work \"&AOk-t&AOk-\") FLAGS (\\Seen) "+
		"BODY[HEADER.FIELDS (FROM SUBJECT)] {22}\r\nSubject: a (b) \"c\"\r\n\r\n X-GM-MSGID 1278455344230334865 ENVELOPE NIL)\r\n"+
		"* OK [UIDVALIDITY 11] UIDs valid.\r\n"+
		"x1 OK [APPENDUID 11 42] (Success)\r\n"+
		"* LIST (\\HasNoChildren \\Drafts) \"/\" \"[Gmail]/Drafts\"\r\n"+
		"+ go ahead\r\n")
	if len(rs) != 5 {
		t.Fatalf("got %d responses", len(rs))
	}
	it := fetchItems(rs[0])
	if rs[0].Num != 3 || rs[0].Kind != "FETCH" || asUint(it["UID"]) != 17 || asUint(it["X-GM-MSGID"]) != 1278455344230334865 {
		t.Fatalf("fetch = %+v", it)
	}
	var labels []string
	for _, l := range asList(it["X-GM-LABELS"]) {
		labels = append(labels, decodeMUTF7(asString(l)))
	}
	if strings.Join(labels, "|") != `\Inbox|Work|été` {
		t.Fatalf("labels = %q", labels)
	}
	if got := string(asBytes(it["BODY[HEADER.FIELDS (FROM SUBJECT)]"])); got != "Subject: a (b) \"c\"\r\n\r\n" {
		t.Fatalf("literal = %q", got)
	}
	if v, ok := it["ENVELOPE"]; !ok || v != nil {
		t.Fatalf("NIL = %#v", v)
	}
	if rs[1].Status != "OK" || rs[1].Code != "UIDVALIDITY" || rs[1].CodeArgs[0] != "11" {
		t.Fatalf("status = %+v", rs[1])
	}
	if rs[2].Tag != "x1" || rs[2].Code != "APPENDUID" || rs[2].CodeArgs[1] != "42" {
		t.Fatalf("tagged = %+v", rs[2])
	}
	if rs[3].Kind != "LIST" || asString(rs[3].Fields[2]) != "[Gmail]/Drafts" {
		t.Fatalf("list = %+v", rs[3])
	}
	if rs[4].Tag != "+" || rs[4].Text != "go ahead" {
		t.Fatalf("continuation = %+v", rs[4])
	}
}

func TestQuoting(t *testing.T) {
	if got := q(`a "b" \c`); got != raw(`"a \"b\" \\c"`) {
		t.Fatalf("quoted = %v", got)
	}
	if _, ok := q("été").(lit); !ok {
		t.Fatal("non-ASCII must be a literal")
	}
	if _, ok := q("a\r\nb").(lit); !ok {
		t.Fatal("CR/LF must be a literal")
	}
}

func TestMUTF7(t *testing.T) {
	for _, s := range []string{"INBOX", "Projets été", "a&b", "日本語/メール", "[Gmail]/Sent Mail"} {
		if got := decodeMUTF7(encodeMUTF7(s)); got != s {
			t.Errorf("%q → %q → %q", s, encodeMUTF7(s), got)
		}
	}
	if encodeMUTF7("a&b") != "a&-b" || encodeMUTF7("été") != "&AOk-t&AOk-" {
		t.Fatalf("encode = %q %q", encodeMUTF7("a&b"), encodeMUTF7("été"))
	}
	if decodeMUTF7("bad&***-") != "bad&***-" {
		t.Fatal("invalid input should pass through")
	}
}

func TestPrepareSubmission(t *testing.T) {
	raw := "From: james@gmail.com\r\nTo: a@example.com\r\nBcc: secret@example.com\r\nSubject: =?utf-8?q?caf=C3=A9?=\r\n" +
		"References: <1@x>\r\n <2@x>\r\nDate: Mon, 1 Jan 2024 00:00:00 +0000\r\nContent-Type: text/plain\r\n\r\nbody\r\n.dot\r\n"
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	out, env, err := prepareSubmission([]byte(raw), "james@gmail.com", "gmail.com", now)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "Bcc") || strings.Contains(s, "2024") || !strings.Contains(s, "Date: Fri, 09 Oct 2026 12:00:00 +0000\r\n") {
		t.Fatalf("headers:\n%s", s)
	}
	if !strings.Contains(s, "References: <1@x>\r\n <2@x>\r\n") || !strings.HasSuffix(s, "\r\n\r\nbody\r\n.dot\r\n") {
		t.Fatalf("folding/body lost:\n%s", s)
	}
	if !validMessageID(env.messageID) || !strings.Contains(s, "Message-ID: "+env.messageID+"\r\n") {
		t.Fatalf("message id = %q", env.messageID)
	}
	if strings.Join(env.rcpt, ",") != "a@example.com,secret@example.com" {
		t.Fatalf("rcpt = %v", env.rcpt)
	}
	// The draft's Message-ID is replaced, so Gmail does not take the sent
	// message for a duplicate of the draft.
	out, env, _ = prepareSubmission([]byte("From: james@gmail.com\r\nTo: a@x.org\r\nMessage-ID: <draft@x>\r\n\r\nb"), "james@gmail.com", "gmail.com", now)
	if env.messageID == "<draft@x>" || strings.Contains(string(out), "<draft@x>") || !strings.HasSuffix(env.messageID, "@gmail.com>") {
		t.Fatalf("message id = %q:\n%s", env.messageID, out)
	}
	if _, _, err := prepareSubmission([]byte("From: james@gmail.com\r\nSubject: x\r\n\r\nb"), "james@gmail.com", "gmail.com", now); err == nil {
		t.Fatal("no recipients accepted")
	}
}

func validMessageID(s string) bool {
	return strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") && strings.Contains(s, "@") &&
		!strings.ContainsAny(s, " \t\r\n")
}

// Values that reach the wire unquoted are checked; everything from a
// caller is quoted or sent as a literal.
func TestNoCommandInjection(t *testing.T) {
	bad := cursor{Box: "INBOX", Max: 5, Terms: []term{{K: "ALL\r\nx9 DELETE INBOX", V: "x"}}}
	if _, err := bad.criteria(false); err == nil {
		t.Fatal("forged cursor key accepted")
	}
	bad = cursor{Box: "INBOX", Max: 5, Terms: []term{{K: "SINCE", V: "2026-01-01\r\nx9 LOGOUT"}}}
	if _, err := bad.criteria(false); err == nil {
		t.Fatal("forged cursor date accepted")
	}
	if got := gmLabels([]string{`\Inbox`, `\Inbox) UID 1:* (`}); got != `(\Inbox "\\Inbox) UID 1:* (")` {
		t.Fatalf("labels = %s", got)
	}
}
