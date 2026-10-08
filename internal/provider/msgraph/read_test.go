package msgraph

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/service"
)

var ctx = context.Background()

func q(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestListPaging(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages", querySub: "skip=1", fixture: "list_inbox_page2.json"})
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages", fixture: "list_inbox_page1.json"})
	p, _ := f.provider(t)

	pg, err := p.List(ctx, provider.ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	req := f.last()
	v := q(t, req.RawQuery)
	if v.Get("$orderby") != "receivedDateTime desc" || v.Get("$top") != "1" || !strings.Contains(v.Get("$select"), "parentFolderId") {
		t.Fatalf("query: %v", v)
	}
	if v.Has("$filter") {
		t.Fatalf("no filter expected: %v", v)
	}
	if strings.Contains(req.RawQuery, "+") {
		t.Fatalf("spaces must be %%20: %s", req.RawQuery)
	}
	if req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("User-Agent") != "email-cli/test" || req.Header.Get("client-request-id") == "" {
		t.Fatalf("headers: %v", req.Header)
	}
	m := pg.Messages[0]
	if m.ID != "AAMkAD-msg-2" || !m.Flags.Unread || !m.Flags.Flagged || !m.Flags.HasAttachments || m.From.Email != "alice@contoso.example" {
		t.Fatalf("message: %+v", m)
	}
	if strings.Join(m.Labels, ",") != "inbox,Finance" {
		t.Fatalf("labels: %v", m.Labels)
	}
	if !strings.HasPrefix(pg.NextCursor, f.base()) {
		t.Fatalf("cursor: %q", pg.NextCursor)
	}

	pg2, err := p.List(ctx, provider.ListOptions{Cursor: pg.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(pg2.Messages) != 1 || pg2.Messages[0].ID != "AAMkAD-msg-1" || pg2.NextCursor != "" {
		t.Fatalf("page 2: %+v", pg2)
	}
}

func TestListUnreadSinceFilterOrder(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages", body: `{"value":[]}`})
	p, _ := f.provider(t)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := p.List(ctx, provider.ListOptions{Unread: true}); err != nil {
		t.Fatal(err)
	}
	if got := q(t, f.last().RawQuery).Get("$filter"); got != "receivedDateTime ge 1900-01-01T00:00:00Z and isRead eq false" {
		t.Fatalf("filter: %q", got)
	}
	if _, err := p.List(ctx, provider.ListOptions{Since: since}); err != nil {
		t.Fatal(err)
	}
	if got := q(t, f.last().RawQuery).Get("$filter"); got != "receivedDateTime ge 2026-10-01T00:00:00Z" {
		t.Fatalf("filter: %q", got)
	}
}

func TestListCategoryWhenNoFolder(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/mailFolders", body: `{"value":[]}`})
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/childFolders", body: `{"value":[]}`})
	f.on(route{method: "GET", path: "/me/messages", body: `{"value":[]}`})
	p, _ := f.provider(t)
	if _, err := p.List(ctx, provider.ListOptions{Label: "Finance"}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if got := q(t, reqs[0].RawQuery).Get("$filter"); got != "displayName eq 'Finance'" {
		t.Fatalf("folder lookup: %q", got)
	}
	if got := q(t, f.last().RawQuery).Get("$filter"); got != "receivedDateTime ge 1900-01-01T00:00:00Z and categories/any(c:c eq 'Finance')" {
		t.Fatalf("category filter: %q", got)
	}
}

func TestReservedLabelMapping(t *testing.T) {
	for label, wk := range map[string]string{"sent": "sentitems", "trash": "deleteditems", "spam": "junkemail", "archive": "archive", "drafts": "drafts"} {
		f := newFakeGraph(t)
		f.on(route{method: "GET", path: "/me/mailFolders/" + wk + "/messages", body: `{"value":[]}`})
		p, _ := f.provider(t)
		if _, err := p.List(ctx, provider.ListOptions{Label: label}); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
}

func TestSearchUsesOnlySearch(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages", fixture: "list_inbox_page2.json"})
	p, _ := f.provider(t)
	pg, err := p.Search(ctx, provider.SearchOptions{Query: `from:alice subject:"q3 numbers"`, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	v := q(t, f.last().RawQuery)
	if v.Get("$search") != `"from:alice subject:\"q3 numbers\""` {
		t.Fatalf("$search: %q", v.Get("$search"))
	}
	if v.Has("$filter") || v.Has("$orderby") {
		t.Fatalf("$search must not be combined: %v", v)
	}
	if len(pg.Messages) != 1 || pg.Messages[0].Labels[0] != "inbox" {
		t.Fatalf("labels from parentFolderId: %+v", pg.Messages)
	}
}

func TestGetHTMLIsSanitizedByService(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages/AAMkAD-msg-2", fixture: "get_html.json"})
	p, _ := f.provider(t)
	m, err := p.Get(ctx, "AAMkAD-msg-2", provider.GetOptions{Headers: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q(t, f.last().RawQuery).Get("$select"), "internetMessageHeaders") {
		t.Fatal("--headers must select internetMessageHeaders")
	}
	if m.BodyHTML == "" || m.BodyText != "" {
		t.Fatalf("provider should return raw HTML: %+v", m)
	}
	if m.Headers["Message-ID"] != "<abc123@contoso.example>" || m.Headers["Authentication-Results"] == "" {
		t.Fatalf("headers: %v", m.Headers)
	}
	if _, leaked := m.Headers["X-MS-Exchange-Organization-SCL"]; leaked {
		t.Fatal("non-allowlisted header returned")
	}
	service.NormalizeBody(m, false, 0)
	if m.BodyText != "Looks good to me." || !m.BodyHiddenTextRemoved || m.BodyHTML != "" {
		t.Fatalf("normalized: %+v", m)
	}
}

func TestGetWithoutHeadersDropsThem(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages/AAMkAD-msg-2", fixture: "get_html.json"})
	p, _ := f.provider(t)
	m, err := p.Get(ctx, "AAMkAD-msg-2", provider.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Headers != nil {
		t.Fatalf("headers: %v", m.Headers)
	}
}

func TestErrorMapping(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages/missing", status: 404, fixture: "error_not_found.json"})
	f.on(route{method: "GET", path: "/me/messages/expired", status: 401, fixture: "error_unauthorized.json"})
	p, _ := f.provider(t)

	_, err := p.Get(ctx, "missing", provider.GetOptions{})
	e := errs.From(err)
	if e.Exit != errs.ExitProvider || e.ProviderCode != "ErrorItemNotFound" || !strings.Contains(e.Message, "request-id req-GET-test") {
		t.Fatalf("404: %+v", e)
	}
	_, err = p.Get(ctx, "expired", provider.GetOptions{})
	e = errs.From(err)
	if e.Exit != errs.ExitAuth || !strings.Contains(e.Message, "email auth login -a work") {
		t.Fatalf("401: %+v", e)
	}
}

func TestRetryAfterThenSuccess(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages/AAMkAD-msg-2", status: 429, fixture: "error_throttled.json", header: map[string]string{"Retry-After": "7"}, times: 2})
	f.on(route{method: "GET", path: "/me/messages/AAMkAD-msg-2", fixture: "get_html.json"})
	p, sleeps := f.provider(t)
	if _, err := p.Get(ctx, "AAMkAD-msg-2", provider.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != 7*time.Second {
		t.Fatalf("sleeps: %v", *sleeps)
	}
	if n := len(f.requests()); n != 3 {
		t.Fatalf("requests: %d", n)
	}
}

func TestRetryGivesUpAfterThree(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages/x", status: 503, fixture: "error_throttled.json"})
	p, sleeps := f.provider(t)
	_, err := p.Get(ctx, "x", provider.GetOptions{})
	if e := errs.From(err); e.Exit != errs.ExitProvider || e.ProviderCode != "ApplicationThrottled" {
		t.Fatalf("err: %+v", e)
	}
	if len(f.requests()) != 4 || len(*sleeps) != 3 || (*sleeps)[2] != 4*time.Second {
		t.Fatalf("requests %d sleeps %v", len(f.requests()), *sleeps)
	}
}

func TestCursorMustStayOnGraphHost(t *testing.T) {
	f := newFakeGraph(t)
	p, _ := f.provider(t)
	for _, cur := range []string{"https://evil.example/v1.0/me/messages", "http://127.0.0.1:1/v1.0/me", f.srv.URL + "/beta/me/messages"} {
		_, err := p.List(ctx, provider.ListOptions{Cursor: cur})
		if errs.ExitCode(err) != errs.ExitUsage {
			t.Errorf("cursor %s: %v", cur, err)
		}
	}
	if len(f.requests()) != 0 {
		t.Fatal("request sent to a foreign cursor")
	}
}

func TestThreadSortedOldestFirst(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages", fixture: "list_inbox_page1.json", times: 1})
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages", fixture: "list_inbox_page2.json"})
	p, _ := f.provider(t)
	msgs, err := p.Thread(ctx, "AAQkAD-conv-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := q(t, f.requests()[0].RawQuery).Get("$filter"); got != "conversationId eq 'AAQkAD-conv-1'" {
		t.Fatalf("filter: %q", got)
	}
	if len(msgs) != 2 || msgs[0].ID != "AAMkAD-msg-1" {
		t.Fatalf("order: %v", msgs)
	}
}
