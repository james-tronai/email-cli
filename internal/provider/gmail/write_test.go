package gmail

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

const draftID = "r-5551234567890123456"

func draftRoutes(f *fakeGmail) {
	f.withLabels()
	f.on(route{method: "POST", path: "/upload" + u + "/drafts", fixture: "draft_created.json"})
	f.on(route{method: "PUT", path: "/upload" + u + "/drafts/" + draftID, fixture: "draft_created.json"})
	f.on(route{method: "GET", path: u + "/drafts/" + draftID, fixture: "draft_full.json"})
	f.on(route{method: "GET", path: u + "/messages/18f0d0d0d0d0d001/attachments/ANGjdJ-draft-att", fixture: "draft_attachment.json"})
	f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f1", fixture: "full_html.json"})
	f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f1/attachments/ANGjdJ-volatile-id-1", fixture: "attachment_q3.json"})
}

type upload struct {
	meta map[string]any
	msg  *mail.Message
	body string            // decoded text/plain
	atts map[string]string // filename → content
}

// parseUpload decodes a multipart/related draft upload.
func parseUpload(t *testing.T, r recorded) upload {
	t.Helper()
	if q(t, r.RawQuery).Get("uploadType") != "multipart" {
		t.Fatalf("uploadType: %s", r.RawQuery)
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/related" {
		t.Fatalf("content type %q", r.Header.Get("Content-Type"))
	}
	mr := multipart.NewReader(strings.NewReader(r.Body), params["boundary"])
	var up upload
	p1, _ := mr.NextPart()
	if err := json.NewDecoder(p1).Decode(&up.meta); err != nil {
		t.Fatal(err)
	}
	p2, _ := mr.NextPart()
	if p2.Header.Get("Content-Type") != "message/rfc822" {
		t.Fatalf("second part: %v", p2.Header)
	}
	if up.msg, err = mail.ReadMessage(p2); err != nil {
		t.Fatal(err)
	}
	up.atts = map[string]string{}
	readPart := func(h mail.Header, body io.Reader) {
		ct, cp, _ := mime.ParseMediaType(h.Get("Content-Type"))
		var data []byte
		switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
		case "quoted-printable":
			data, _ = io.ReadAll(newQP(body))
		case "base64":
			data, _ = io.ReadAll(newB64(body))
		default:
			data, _ = io.ReadAll(body)
		}
		if _, dp, _ := mime.ParseMediaType(h.Get("Content-Disposition")); dp["filename"] != "" {
			up.atts[dp["filename"]] = string(data)
			return
		}
		if ct == "text/plain" {
			if cp["charset"] != "UTF-8" {
				t.Errorf("charset %q", cp["charset"])
			}
			up.body = string(data)
		}
	}
	ct, cp, _ := mime.ParseMediaType(up.msg.Header.Get("Content-Type"))
	if ct == "multipart/mixed" {
		parts := multipart.NewReader(up.msg.Body, cp["boundary"])
		for {
			pt, err := parts.NextRawPart()
			if err != nil {
				break
			}
			readPart(mail.Header(pt.Header), pt)
		}
	} else {
		readPart(up.msg.Header, up.msg.Body)
	}
	return up
}

func TestCreateDraftMIME(t *testing.T) {
	f := newFakeGmail(t)
	draftRoutes(f)
	p, _ := f.provider(t)
	m, err := p.CreateDraft(ctx, provider.DraftInput{
		To:          []provider.Address{{Name: "Zoë Q", Email: "zoe@contoso.example"}},
		Bcc:         []provider.Address{{Email: "audit@tronai.example"}},
		Subject:     "Résumé\r\nBcc: evil@attacker.example",
		Body:        "Line one — ünïcode\nLine two",
		Attachments: []provider.AttachmentFile{{Name: "notes.txt", MIME: "text/plain", Data: []byte("hello")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := parseUpload(t, f.one("POST", "/upload"+u+"/drafts"))
	if _, ok := up.meta["message"].(map[string]any)["threadId"]; ok {
		t.Fatal("a new draft has no threadId")
	}
	h := up.msg.Header
	dec := new(mime.WordDecoder)
	subj, _ := dec.DecodeHeader(h.Get("Subject"))
	if subj != "Résumé  Bcc: evil@attacker.example" || len(h["Bcc"]) != 1 {
		t.Fatalf("header injection: subject %q bcc %v", subj, h["Bcc"])
	}
	to, _ := h.AddressList("To")
	if h.Get("From") != "<james@tronai.example>" || to[0].Name != "Zoë Q" || h.Get("Bcc") != "<audit@tronai.example>" {
		t.Fatalf("addresses: from %q to %v bcc %q", h.Get("From"), to, h.Get("Bcc"))
	}
	if up.body != "Line one — ünïcode\r\nLine two" || up.atts["notes.txt"] != "hello" {
		t.Fatalf("body %q atts %v", up.body, up.atts)
	}
	if m.ID != draftID || !m.Flags.Draft || m.WebLink != "https://mail.google.com/mail/u/0/#drafts?compose=18f0d0d0d0d0d001" {
		t.Fatalf("result: id %q link %q", m.ID, m.WebLink)
	}
	if m.To[0].Email != "alice.replies@contoso.example" || m.Headers != nil {
		t.Fatalf("result is read back from the server: %+v", m)
	}
}

func TestReplyDraftThreading(t *testing.T) {
	f := newFakeGmail(t)
	draftRoutes(f)
	p, _ := f.provider(t)
	if _, err := p.CreateReplyDraft(ctx, provider.ReplyInput{MessageID: "18f0a1b2c3d4e5f1", All: true, Body: "Will do."}); err != nil {
		t.Fatal(err)
	}
	up := parseUpload(t, f.one("POST", "/upload"+u+"/drafts"))
	if up.meta["message"].(map[string]any)["threadId"] != "18f0a1b2c3d4e5f0" {
		t.Fatalf("threadId: %v", up.meta)
	}
	h := up.msg.Header
	if h.Get("In-Reply-To") != "<CAF=abc123@mail.contoso.example>" ||
		h.Get("References") != "<CAF=root000@mail.contoso.example> <CAF=abc123@mail.contoso.example>" {
		t.Fatalf("threading headers: %v", h)
	}
	if h.Get("Subject") != "Re: Q3 numbers" {
		t.Fatalf("subject %q", h.Get("Subject"))
	}
	// Reply-To wins over From; reply-all adds To/Cc minus ourselves.
	if h.Get("To") != "<alice.replies@contoso.example>" || h.Get("Cc") != "<bob@contoso.example>" {
		t.Fatalf("recipients: to %q cc %q", h.Get("To"), h.Get("Cc"))
	}
	if !strings.HasPrefix(up.body, "Will do.\r\n\r\nOn Thu, 9 Oct 2025 08:53:20 +0000, Alice Example <alice@contoso.example> wrote:\r\n> Hi James,") {
		t.Fatalf("body: %q", up.body)
	}
	if strings.Contains(up.body, "attacker") || strings.Contains(up.body, "IGNORE") {
		t.Fatalf("hidden text quoted into the reply: %q", up.body)
	}
	if len(up.atts) != 0 {
		t.Fatalf("a reply does not carry the original's attachments: %v", up.atts)
	}

	if _, err := p.CreateReplyDraft(ctx, provider.ReplyInput{MessageID: draftID, Body: "x"}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("reply to a draft: %v", err)
	}
}

func TestForwardDraftCarriesAttachments(t *testing.T) {
	f := newFakeGmail(t)
	draftRoutes(f)
	p, _ := f.provider(t)
	if _, err := p.CreateForwardDraft(ctx, provider.ForwardInput{MessageID: "18f0a1b2c3d4e5f1", To: []provider.Address{{Email: "colleague@tronai.example"}}, Body: "FYI"}); err != nil {
		t.Fatal(err)
	}
	up := parseUpload(t, f.one("POST", "/upload"+u+"/drafts"))
	if up.msg.Header.Get("Subject") != "Fwd: Q3 numbers" || up.msg.Header.Get("To") != "<colleague@tronai.example>" {
		t.Fatalf("headers: %v", up.msg.Header)
	}
	if !strings.HasPrefix(up.body, "FYI\r\n\r\n---------- Forwarded message ---------\r\nFrom: Alice Example <alice@contoso.example>") {
		t.Fatalf("body: %q", up.body)
	}
	if up.atts["q3.pdf"] != "%PDF-1.4 q3" || len(up.atts) != 1 {
		t.Fatalf("attachments (inline logo excluded): %v", up.atts)
	}
}

func TestUpdateDraftKeepsUnchangedFields(t *testing.T) {
	f := newFakeGmail(t)
	draftRoutes(f)
	f.on(route{method: "GET", path: u + "/drafts", fixture: "drafts_list.json"})
	p, _ := f.provider(t)
	body := "New text"
	// Addressed by the draft's message id, as `list --label drafts` returns it.
	if _, err := p.UpdateDraft(ctx, "18f0d0d0d0d0d001", provider.DraftUpdate{Body: &body}); err != nil {
		t.Fatal(err)
	}
	up := parseUpload(t, f.one("PUT", "/upload"+u+"/drafts/"+draftID))
	if up.meta["id"] != draftID || up.meta["message"].(map[string]any)["threadId"] != "18f0a1b2c3d4e5f0" {
		t.Fatalf("meta: %v", up.meta)
	}
	h := up.msg.Header
	if h.Get("To") != "<alice.replies@contoso.example>" || h.Get("Bcc") != "<audit@tronai.example>" || h.Get("Subject") != "Re: Q3 numbers" ||
		h.Get("In-Reply-To") != "<CAF=abc123@mail.contoso.example>" {
		t.Fatalf("carried headers: %v", h)
	}
	if up.body != "New text" || up.atts["numbers.csv"] != "a,b\n1,2" {
		t.Fatalf("body %q atts %v", up.body, up.atts)
	}

	if _, err := p.UpdateDraft(ctx, "18f0a1b2c3d4e5f1", provider.DraftUpdate{Body: &body}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("updating a non-draft: %v", err)
	}
}

func TestDraftListAndDelete(t *testing.T) {
	f := newFakeGmail(t)
	draftRoutes(f)
	f.on(route{method: "GET", path: u + "/drafts", fixture: "drafts_list.json"})
	f.on(route{method: "DELETE", path: u + "/drafts/" + draftID, status: 204})
	p, _ := f.provider(t)
	pg, err := p.ListDrafts(ctx, provider.ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Messages) != 1 || pg.Messages[0].ID != draftID || !strings.Contains(pg.Messages[0].WebLink, "compose=18f0d0d0d0d0d001") || pg.NextCursor == "" {
		t.Fatalf("draft list: %+v", pg)
	}
	if q(t, f.find("GET", u+"/drafts/"+draftID)[0].RawQuery).Get("format") != "metadata" {
		t.Fatal("draft list should fetch metadata only")
	}
	if _, err := p.List(ctx, provider.ListOptions{Cursor: pg.NextCursor}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("draft cursor used with list: %v", err)
	}
	if err := p.DeleteDraft(ctx, draftID); err != nil {
		t.Fatal(err)
	}
	f.one("DELETE", u+"/drafts/"+draftID)
}

func TestMoveLabelMark(t *testing.T) {
	f := newFakeGmail(t).withLabels()
	f.on(route{method: "POST", path: u + "/messages/18f0a1b2c3d4e5f1/modify", fixture: "modify_result.json"})
	f.on(route{method: "POST", path: u + "/labels", fixture: "label_created.json"})
	f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f1", fixture: "meta_1.json"})
	p, _ := f.provider(t)

	modifies := func() []map[string][]string {
		var out []map[string][]string
		for _, r := range f.find("POST", u+"/messages/18f0a1b2c3d4e5f1/modify") {
			var b map[string][]string
			_ = json.Unmarshal([]byte(r.Body), &b)
			out = append(out, b)
		}
		return out
	}
	check := func(i int, add, rm string) {
		t.Helper()
		b := modifies()[i]
		if strings.Join(b["addLabelIds"], ",") != add || strings.Join(b["removeLabelIds"], ",") != rm {
			t.Fatalf("modify %d: %v", i, b)
		}
	}
	m, err := p.Move(ctx, "18f0a1b2c3d4e5f1", "archive")
	if err != nil || m.ID != "18f0a1b2c3d4e5f1" {
		t.Fatalf("move: %v %+v", err, m)
	}
	check(0, "", "INBOX,SPAM")
	if _, err := p.Move(ctx, "18f0a1b2c3d4e5f1", "Finance"); err != nil {
		t.Fatal(err)
	}
	check(1, "Label_7", "INBOX")
	if _, err := p.Relabel(ctx, "18f0a1b2c3d4e5f1", []string{"Waiting", "inbox"}, []string{"finance", "never-existed"}); err != nil {
		t.Fatal(err)
	}
	check(2, "Label_20,INBOX", "Label_7")
	var created map[string]string
	_ = json.Unmarshal([]byte(f.one("POST", u+"/labels").Body), &created)
	if created["name"] != "Waiting" {
		t.Fatalf("label create: %v", created)
	}
	if _, err := p.Mark(ctx, "18f0a1b2c3d4e5f1", provider.MarkOptions{Read: ptr(true), Flagged: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	check(3, "STARRED", "UNREAD")

	for _, bad := range []string{"archive", "trash", "sent"} {
		if _, err := p.Relabel(ctx, "18f0a1b2c3d4e5f1", []string{bad}, nil); errs.ExitCode(err) != errs.ExitUsage {
			t.Fatalf("label --add %s: %v", bad, err)
		}
	}
	if _, err := p.Move(ctx, draftID, "archive"); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("moving a draft: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestSyncHistory(t *testing.T) {
	f := newFakeGmail(t).withLabels()
	f.on(route{method: "GET", path: u + "/profile", fixture: "profile.json"})
	f.on(route{method: "GET", path: u + "/history", querySub: "startHistoryId=500100", fixture: "history.json"})
	f.on(route{method: "GET", path: u + "/history", querySub: "startHistoryId=500110", fixture: "history_empty.json"})
	f.on(route{method: "GET", path: u + "/history", querySub: "startHistoryId=1", status: 404, fixture: "error_404.json"})
	f.on(route{method: "GET", path: u + "/messages/18f0e0e0e0e0e002", fixture: "meta_new.json"})
	p, _ := f.provider(t)

	res, err := p.Sync(ctx, provider.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Initialized || len(res.Messages) != 0 || res.State != `{"history_id":"500100","label_id":"INBOX"}` {
		t.Fatalf("baseline: %+v", res)
	}
	res, err = p.Sync(ctx, provider.SyncOptions{State: res.State})
	if err != nil {
		t.Fatal(err)
	}
	v := q(t, f.find("GET", u+"/history")[0].RawQuery)
	if v.Get("historyTypes") != "messageAdded" || v.Get("labelId") != "INBOX" {
		t.Fatalf("history query: %v", v)
	}
	// The draft is skipped and the repeated add is reported once.
	if res.Initialized || len(res.Messages) != 1 || res.Messages[0].ID != "18f0e0e0e0e0e002" || res.Messages[0].Subject != "New one" {
		t.Fatalf("round 2: %+v", res)
	}
	if res.State != `{"history_id":"500110","label_id":"INBOX"}` {
		t.Fatalf("state: %s", res.State)
	}
	res, _ = p.Sync(ctx, provider.SyncOptions{State: res.State})
	if len(res.Messages) != 0 || !strings.Contains(res.State, "500120") {
		t.Fatalf("round 3: %+v", res)
	}

	res, err = p.Sync(ctx, provider.SyncOptions{State: `{"history_id":"1","label_id":"INBOX"}`})
	if err != nil || !res.Initialized || !strings.Contains(res.State, "500100") {
		t.Fatalf("expired history id should rebaseline: %v %+v", err, res)
	}
	if _, err := p.Sync(ctx, provider.SyncOptions{State: `{"delta":"https://graph…"}`}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("foreign state: %v", err)
	}
	if _, err := p.Sync(ctx, provider.SyncOptions{Label: "finance", State: `{"history_id":"500100","label_id":"INBOX"}`}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("state for another label: %v", err)
	}
}

func TestRawPathRestriction(t *testing.T) {
	f := newFakeGmail(t).withLabels()
	p, _ := f.provider(t)
	for _, ok := range []string{"/gmail/v1/users/me/labels", "/gmail/v1/users/James@tronai.example/profile", "/upload/gmail/v1/users/me/drafts?uploadType=media"} {
		if err := p.checkRawPath(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/gmail/v1/users/boss@tronai.example/messages", "/gmail/v1/users/me/../other/labels", "/oauth2/v2/userinfo", "/gmail/v1/users/meX"} {
		if err := p.checkRawPath(bad); err == nil {
			t.Errorf("%s allowed", bad)
		}
	}
	res, err := p.Raw(ctx, provider.RawRequest{Method: "GET", Path: "/gmail/v1/users/me/labels"})
	if err != nil || res.Status != 200 || !strings.Contains(string(res.Body), "Finance") {
		t.Fatalf("raw: %v %+v", err, res)
	}
}

func newQP(r io.Reader) io.Reader  { return quotedprintable.NewReader(r) }
func newB64(r io.Reader) io.Reader { return base64.NewDecoder(base64.StdEncoding, r) }

func TestSendDraft(t *testing.T) {
	f := newFakeGmail(t)
	f.on(route{method: "GET", path: u + "/drafts", fixture: "drafts_list.json"})
	f.on(route{method: "POST", path: u + "/drafts/send", body: `{"id":"18f0f0f0f0f0f0f1","threadId":"18f0a1b2c3d4e5f0","labelIds":["SENT"]}`})
	f.on(route{method: "GET", path: u + "/messages/18f0f0f0f0f0f0f1", body: `{"id":"18f0f0f0f0f0f0f1","payload":{"headers":[{"name":"Message-ID","value":"<CAsent@mail.gmail.com>"}]}}`})
	p, _ := f.provider(t)

	// Addressed by the draft's message id; drafts.send needs the draft id.
	res, err := p.Send(ctx, "18f0d0d0d0d0d001")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(f.one("POST", u+"/drafts/send").Body), &body)
	if body["id"] != draftID {
		t.Fatalf("send body: %v", body)
	}
	if res.MessageID != "18f0f0f0f0f0f0f1" || res.ThreadID != "18f0a1b2c3d4e5f0" || res.InternetMessageID != "<CAsent@mail.gmail.com>" {
		t.Fatalf("result: %+v", res)
	}
	if _, err := p.Send(ctx, "18f0a1b2c3d4e5f1"); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("sending a non-draft: %v", err)
	}
	if len(f.find("POST", u+"/drafts/send")) != 1 {
		t.Fatal("a non-draft was sent")
	}
}
