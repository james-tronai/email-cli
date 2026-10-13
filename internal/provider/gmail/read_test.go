package gmail

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/service"
)

func listRoutes(f *fakeGmail) {
	f.withLabels()
	f.on(route{method: "GET", path: u + "/messages", querySub: "pageToken=token-page-2", fixture: "list_inbox_page2.json"})
	f.on(route{method: "GET", path: u + "/messages", fixture: "list_inbox_page1.json"})
	for _, n := range []string{"1", "2", "3"} {
		f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f" + n, fixture: "meta_" + n + ".json"})
	}
}

func TestListPagingAndConvert(t *testing.T) {
	f := newFakeGmail(t)
	listRoutes(f)
	p, _ := f.provider(t)

	pg, err := p.List(ctx, provider.ListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	req := f.one("GET", u+"/messages")
	v := q(t, req.RawQuery)
	if v.Get("labelIds") != "INBOX" || v.Get("maxResults") != "2" || v.Has("q") || v.Has("includeSpamTrash") {
		t.Fatalf("list query: %v", v)
	}
	if req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("User-Agent") != "email-cli/test" {
		t.Fatalf("headers: %v", req.Header)
	}
	meta := q(t, f.one("GET", u+"/messages/18f0a1b2c3d4e5f1").RawQuery)
	if meta.Get("format") != "metadata" || strings.Join(meta["metadataHeaders"], ",") != "From,To,Cc,Reply-To,Subject,Date" {
		t.Fatalf("metadata query: %v", meta)
	}
	if len(pg.Messages) != 2 || pg.Messages[0].ID != "18f0a1b2c3d4e5f1" || pg.Messages[1].ID != "18f0a1b2c3d4e5f2" {
		t.Fatalf("order: %+v", pg.Messages)
	}
	m := pg.Messages[0]
	if m.ThreadID != "18f0a1b2c3d4e5f0" || m.Provider != "gmail" || m.Account != "gw" {
		t.Fatalf("ids: %+v", m)
	}
	if m.From.Name != "Alice Example" || m.From.Email != "alice@contoso.example" || m.Cc[0].Name != "Björn" {
		t.Fatalf("addresses: %+v %+v", m.From, m.Cc)
	}
	if m.Subject != "Q3 numbers — urgent" || m.Snippet != "Can you check the Q3 numbers & send them by Friday? It's urgent" {
		t.Fatalf("subject/snippet: %q / %q", m.Subject, m.Snippet)
	}
	if !m.Flags.Unread || !m.Flags.Flagged || !m.Flags.HasAttachments || m.Flags.Draft {
		t.Fatalf("flags: %+v", m.Flags)
	}
	if strings.Join(m.Labels, ",") != "inbox,Finance" {
		t.Fatalf("labels: %v", m.Labels)
	}
	if !m.Date.Equal(time.UnixMilli(1760000000000)) {
		t.Fatalf("date: %v", m.Date)
	}
	if m.WebLink != "https://mail.google.com/mail/u/0/#inbox/18f0a1b2c3d4e5f1" {
		t.Fatalf("web link: %q", m.WebLink)
	}
	if m.Headers != nil {
		t.Fatalf("list must not return headers: %v", m.Headers)
	}
	if got := pg.Messages[1].Labels; strings.Join(got, ",") != "archive" {
		t.Fatalf("message with no folder label should be archive: %v", got)
	}

	if !strings.HasPrefix(pg.NextCursor, cursorPrefix) {
		t.Fatalf("cursor: %q", pg.NextCursor)
	}
	pg2, err := p.List(ctx, provider.ListOptions{Cursor: pg.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	v2 := q(t, f.find("GET", u+"/messages")[1].RawQuery)
	if v2.Get("pageToken") != "token-page-2" || v2.Get("labelIds") != "INBOX" || v2.Get("maxResults") != "2" {
		t.Fatalf("page 2 query must repeat the original query: %v", v2)
	}
	if len(pg2.Messages) != 1 || pg2.NextCursor != "" {
		t.Fatalf("page 2: %+v", pg2)
	}
}

func TestListFilters(t *testing.T) {
	f := newFakeGmail(t)
	listRoutes(f)
	p, _ := f.provider(t)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	if _, err := p.List(ctx, provider.ListOptions{Label: "finance", Unread: true, Since: since}); err != nil {
		t.Fatal(err)
	}
	v := q(t, f.find("GET", u+"/messages")[0].RawQuery)
	if strings.Join(v["labelIds"], ",") != "Label_7,UNREAD" || v.Get("q") != "after:1790812799" {
		t.Fatalf("filters: %v", v)
	}

	if _, err := p.List(ctx, provider.ListOptions{Label: "archive"}); err != nil {
		t.Fatal(err)
	}
	v = q(t, f.find("GET", u+"/messages")[1].RawQuery)
	if v.Has("labelIds") || v.Get("q") != archiveQuery {
		t.Fatalf("archive: %v", v)
	}

	if _, err := p.List(ctx, provider.ListOptions{Label: "trash"}); err != nil {
		t.Fatal(err)
	}
	v = q(t, f.find("GET", u+"/messages")[2].RawQuery)
	if v.Get("labelIds") != "TRASH" || v.Get("includeSpamTrash") != "true" {
		t.Fatalf("trash: %v", v)
	}

	if _, err := p.List(ctx, provider.ListOptions{Label: "nope"}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("unknown label: %v", err)
	}
	if _, err := p.List(ctx, provider.ListOptions{Cursor: "AAMk-graph-cursor"}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("foreign cursor: %v", err)
	}
}

func TestSearchPassesQueryThrough(t *testing.T) {
	f := newFakeGmail(t)
	listRoutes(f)
	p, _ := f.provider(t)
	if _, err := p.Search(ctx, provider.SearchOptions{Query: `from:alice has:attachment "Q3 numbers"`, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	v := q(t, f.one("GET", u+"/messages").RawQuery)
	if v.Get("q") != `from:alice has:attachment "Q3 numbers"` || v.Has("labelIds") || v.Get("maxResults") != "5" {
		t.Fatalf("search: %v", v)
	}
	if _, err := p.Search(ctx, provider.SearchOptions{Query: "  "}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("empty query: %v", err)
	}
}

func TestGetPrefersHTMLAndStripsHidden(t *testing.T) {
	f := newFakeGmail(t).withLabels()
	f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f1", fixture: "full_html.json"})
	p, _ := f.provider(t)

	m, err := p.Get(ctx, "18f0a1b2c3d4e5f1", provider.GetOptions{Headers: true})
	if err != nil {
		t.Fatal(err)
	}
	if q(t, f.one("GET", u+"/messages/18f0a1b2c3d4e5f1").RawQuery).Get("format") != "full" {
		t.Fatal("get must use format=full")
	}
	if m.Headers["Message-Id"] != "<CAF=abc123@mail.contoso.example>" || m.Headers["Authentication-Results"] == "" || m.Headers["X-Secret-Header"] != "" {
		t.Fatalf("headers: %v", m.Headers)
	}
	service.NormalizeBody(m, false, 0)
	if strings.Contains(m.BodyText, "IGNORE") || strings.Contains(m.BodyText, "attacker") || !m.BodyHiddenTextRemoved {
		t.Fatalf("body must come from HTML with hidden text removed: %q", m.BodyText)
	}
	if !strings.Contains(m.BodyText, "Q3 numbers & send them by Friday") || m.BodyHTML != "" {
		t.Fatalf("body: %q", m.BodyText)
	}
	if len(m.Attachments) != 2 || m.Attachments[0].ID != "1" || m.Attachments[0].Name != "q3.pdf" || m.Attachments[0].Inline ||
		!m.Attachments[1].Inline || !m.Flags.HasAttachments {
		t.Fatalf("attachments: %+v", m.Attachments)
	}

	m, _ = p.Get(ctx, "18f0a1b2c3d4e5f1", provider.GetOptions{})
	if m.Headers != nil {
		t.Fatalf("headers without --headers: %v", m.Headers)
	}
}

func TestGetDecodesCharsets(t *testing.T) {
	f := newFakeGmail(t).withLabels()
	f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f4", fixture: "full_latin1.json"})
	p, _ := f.provider(t)
	m, err := p.Get(ctx, "18f0a1b2c3d4e5f4", provider.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m.BodyText != "Café crème à 10h" || m.Subject != "Café" {
		t.Fatalf("charset: body %q subject %q", m.BodyText, m.Subject)
	}
	if strings.Join(m.Labels, ",") != "sent" {
		t.Fatalf("labels: %v", m.Labels)
	}
}

func TestAttachmentsUseStablePartIDs(t *testing.T) {
	f := newFakeGmail(t).withLabels()
	f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f1", fixture: "full_html.json"})
	f.on(route{method: "GET", path: u + "/messages/18f0a1b2c3d4e5f1/attachments/ANGjdJ-volatile-id-1", fixture: "attachment_q3.json"})
	p, _ := f.provider(t)

	metas, err := p.ListAttachments(ctx, "18f0a1b2c3d4e5f1")
	if err != nil || len(metas) != 2 {
		t.Fatalf("list: %v %+v", err, metas)
	}
	a, err := p.GetAttachment(ctx, "18f0a1b2c3d4e5f1", "1")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Data) != "%PDF-1.4 q3" || a.Name != "q3.pdf" || a.MIME != "application/pdf" {
		t.Fatalf("attachment: %+v %q", a.AttachmentMeta, a.Data)
	}
	inl, err := p.GetAttachment(ctx, "18f0a1b2c3d4e5f1", "2")
	if err != nil || string(inl.Data) != "\x89PNG" {
		t.Fatalf("inline data part: %v %q", err, inl.Data)
	}
	if _, err := p.GetAttachment(ctx, "18f0a1b2c3d4e5f1", "0.1"); errs.ExitCode(err) != errs.ExitProvider {
		t.Fatalf("a body part is not an attachment: %v", err)
	}
}

func TestThreadOldestFirst(t *testing.T) {
	f := newFakeGmail(t).withLabels()
	f.on(route{method: "GET", path: u + "/threads/18f0a1b2c3d4e5f0", fixture: "thread.json"})
	p, _ := f.provider(t)
	msgs, err := p.Thread(ctx, "18f0a1b2c3d4e5f0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].BodyText != "earlier" || msgs[1].BodyText != "later" {
		t.Fatalf("order: %+v", msgs)
	}
	msgs, _ = p.Thread(ctx, "18f0a1b2c3d4e5f0", 1)
	if len(msgs) != 1 || msgs[0].BodyText != "later" {
		t.Fatalf("limit keeps the newest: %+v", msgs)
	}
}

func TestErrorsAndRetries(t *testing.T) {
	f := newFakeGmail(t)
	f.on(route{method: "GET", path: u + "/labels", status: 403, fixture: "error_ratelimit.json", times: 2})
	f.on(route{method: "GET", path: u + "/labels", fixture: "labels.json"})
	f.on(route{method: "GET", path: u + "/messages/expired", status: 401, fixture: "error_401.json"})
	f.on(route{method: "POST", path: u + "/messages/18f0a1b2c3d4e5f1/modify", status: 403, fixture: "error_scope.json"})
	p, sleeps := f.provider(t)

	if _, err := p.loadLabels(ctx, false); err != nil {
		t.Fatalf("rate limit should be retried: %v", err)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != time.Second || (*sleeps)[1] != 2*time.Second {
		t.Fatalf("backoff: %v", *sleeps)
	}
	_, err := p.Get(ctx, "expired", provider.GetOptions{})
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "email auth login -a gw --provider gmail") {
		t.Fatalf("401: %v", err)
	}
	_, err = p.Mark(ctx, "18f0a1b2c3d4e5f1", provider.MarkOptions{Read: new(bool)})
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "--mode") {
		t.Fatalf("scope 403: %v", err)
	}
	_, err = p.Get(ctx, "18f0ffffffffffff", provider.GetOptions{})
	var e *errs.Error
	if errs.ExitCode(err) != errs.ExitProvider || !isNotFound(err) || !errors.As(err, &e) || e.ProviderCode != "notFound" || e.Code != "not_found" {
		t.Fatalf("404: %v", err)
	}
}
