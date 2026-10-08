package msgraph

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

func TestCreateReplyAllUsesComment(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "POST", path: "/me/messages/AAMkAD-msg-2/createReplyAll", status: 201, fixture: "create_reply.json"})
	p, _ := f.provider(t)
	m, err := p.CreateReplyDraft(ctx, provider.ReplyInput{MessageID: "AAMkAD-msg-2", All: true, Body: "Thanks Alice"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(f.last().Body), &body)
	if body["comment"] != "Thanks Alice" || len(body) != 1 {
		t.Fatalf("request body: %v", body)
	}
	if !strings.Contains(m.WebLink, "ItemID=AAMkAD-draft-9") {
		t.Fatalf("reply draft webLink: %q", m.WebLink)
	}
	if !m.Flags.Draft || m.ID != "AAMkAD-draft-9" || m.Labels[0] != "drafts" || m.Cc[0].Email != "bob@tronai.example" {
		t.Fatalf("draft: %+v", m)
	}
	if len(f.requests()) != 1 {
		t.Fatalf("expected a single request, got %d", len(f.requests()))
	}
}

func TestCreateDraftWithAttachment(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "POST", path: "/me/messages", status: 201, body: `{"id":"d1","isDraft":true}`})
	f.on(route{method: "POST", path: "/me/messages/d1/attachments", status: 201, body: `{}`})
	f.on(route{method: "GET", path: "/me/messages/d1", body: `{"id":"d1","isDraft":true,"hasAttachments":true,"parentFolderId":"AAMkAD-folder-drafts"}`})
	p, _ := f.provider(t)
	m, err := p.CreateDraft(ctx, provider.DraftInput{
		To: []provider.Address{{Name: "A", Email: "a@tronai.example"}}, Subject: "s", Body: "hello",
		Attachments: []provider.AttachmentFile{{Name: "x.txt", MIME: "text/plain", Data: []byte("hi")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	var create map[string]any
	_ = json.Unmarshal([]byte(reqs[0].Body), &create)
	if create["body"].(map[string]any)["contentType"] != "Text" || create["subject"] != "s" {
		t.Fatalf("create: %v", create)
	}
	var att map[string]any
	_ = json.Unmarshal([]byte(reqs[1].Body), &att)
	if att["@odata.type"] != "#microsoft.graph.fileAttachment" || att["contentBytes"] != "aGk=" {
		t.Fatalf("attachment: %v", att)
	}
	if !m.Flags.HasAttachments {
		t.Fatalf("draft not re-read: %+v", m)
	}
}

func TestLargeAttachmentUnsupportedBeforeAnyRequest(t *testing.T) {
	f := newFakeGraph(t)
	p, _ := f.provider(t)
	_, err := p.CreateDraft(ctx, provider.DraftInput{Attachments: []provider.AttachmentFile{{Name: "big.bin", Data: make([]byte, MaxSimpleAttachment)}}})
	if errs.From(err).Code != "unsupported" || len(f.requests()) != 0 {
		t.Fatalf("err %v, requests %d", err, len(f.requests()))
	}
}

func TestUpdateAndDeleteRequireDraft(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/messages/sent1", body: `{"isDraft":false}`})
	f.on(route{method: "GET", path: "/me/messages/d1", body: `{"isDraft":true}`})
	f.on(route{method: "PATCH", path: "/me/messages/d1", body: `{"id":"d1","subject":"new","isDraft":true}`})
	f.on(route{method: "DELETE", path: "/me/messages/d1", status: 204})
	p, _ := f.provider(t)
	subj := "new"
	if _, err := p.UpdateDraft(ctx, "sent1", provider.DraftUpdate{Subject: &subj}); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("update non-draft: %v", err)
	}
	if err := p.DeleteDraft(ctx, "sent1"); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("delete non-draft: %v", err)
	}
	for _, r := range f.requests() {
		if r.Method != "GET" {
			t.Fatalf("wrote to a non-draft: %s %s", r.Method, r.Path)
		}
	}
	m, err := p.UpdateDraft(ctx, "d1", provider.DraftUpdate{Subject: &subj})
	if err != nil || m.Subject != "new" {
		t.Fatalf("update: %v %+v", err, m)
	}
	if body := f.last().Body; body != `{"subject":"new"}` {
		t.Fatalf("patch body: %s", body)
	}
	if err := p.DeleteDraft(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
}

func TestMoveAndLabelAndMark(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "POST", path: "/me/messages/m1/move", status: 201, body: `{"id":"m1-new"}`})
	f.on(route{method: "GET", path: "/me/messages/m1", body: `{"categories":["Old","Keep"]}`})
	f.on(route{method: "PATCH", path: "/me/messages/m1", body: `{"id":"m1","parentFolderId":"AAMkAD-folder-inbox","categories":["Keep","New"]}`})
	p, _ := f.provider(t)

	m, err := p.Move(ctx, "m1", "Archive")
	if err != nil || m.ID != "m1-new" || m.Labels[0] != "archive" {
		t.Fatalf("move: %v %+v", err, m)
	}
	if body := f.last().Body; body != `{"destinationId":"archive"}` {
		t.Fatalf("move body: %s", body)
	}
	if _, err := p.Relabel(ctx, "m1", []string{"New"}, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	if body := f.last().Body; body != `{"categories":["Keep","New"]}` {
		t.Fatalf("label body: %s", body)
	}
	if _, err := p.Relabel(ctx, "m1", []string{"inbox"}, nil); errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("reserved label: %v", err)
	}
	read, flag := true, true
	if _, err := p.Mark(ctx, "m1", provider.MarkOptions{Read: &read, Flagged: &flag}); err != nil {
		t.Fatal(err)
	}
	if body := f.last().Body; body != `{"flag":{"flagStatus":"flagged"},"isRead":true}` {
		t.Fatalf("mark body: %s", body)
	}
}

func TestDeltaSyncRounds(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages/delta", querySub: "deltatoken=round1", fixture: "delta_round2.json"})
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages/delta", querySub: "skiptoken=round2b", fixture: "delta_round2b.json"})
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages/delta", querySub: "deltatoken=round2", fixture: "delta_round3.json"})
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages/delta", fixture: "delta_baseline.json"})
	p, _ := f.provider(t)

	// Round 1: baseline only.
	r1, err := p.Sync(ctx, provider.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Initialized || len(r1.Messages) != 0 {
		t.Fatalf("baseline: %+v", r1)
	}
	base := f.last()
	if !strings.HasPrefix(q(t, base.RawQuery).Get("$filter"), "receivedDateTime ge ") || base.Header.Get("Prefer") != "odata.maxpagesize=50" {
		t.Fatalf("baseline request: %s %v", base.RawQuery, base.Header)
	}
	var st syncState
	_ = json.Unmarshal([]byte(r1.State), &st)
	if !strings.Contains(st.DeltaLink, "round1") || time.Since(st.Watermark) > time.Minute {
		t.Fatalf("state: %+v", st)
	}

	// Round 2: two pages; new mail reported, read-state change and removal ignored.
	r2, err := p.Sync(ctx, provider.SyncOptions{State: r1.State})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Initialized || len(r2.Messages) != 2 || r2.Messages[0].ID != "AAMkAD-new-1" || r2.Messages[1].ID != "AAMkAD-new-2" {
		t.Fatalf("round 2: %+v", r2.Messages)
	}

	// Round 3: a change to already-reported mail is not new.
	r3, err := p.Sync(ctx, provider.SyncOptions{State: r2.State})
	if err != nil {
		t.Fatal(err)
	}
	if len(r3.Messages) != 0 {
		t.Fatalf("round 3: %+v", r3.Messages)
	}
	_ = json.Unmarshal([]byte(r3.State), &st)
	if !strings.Contains(st.DeltaLink, "round3") {
		t.Fatalf("state not advanced: %+v", st)
	}
}

func TestDeltaResetRebaselines(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages/delta", querySub: "deltatoken=stale", status: 410,
		body: `{"error":{"code":"SyncStateNotFound","message":"The sync state was not found"}}`})
	f.on(route{method: "GET", path: "/me/mailFolders/inbox/messages/delta", fixture: "delta_baseline.json"})
	p, _ := f.provider(t)
	state := `{"delta":"` + f.base() + `/me/mailFolders/inbox/messages/delta?%24deltatoken=stale","watermark":"2026-01-01T00:00:00Z"}`
	r, err := p.Sync(ctx, provider.SyncOptions{State: state})
	if err != nil || !r.Initialized {
		t.Fatalf("reset: %v %+v", err, r)
	}
}

func TestRawPathRestriction(t *testing.T) {
	f := newFakeGraph(t)
	f.on(route{method: "GET", path: "/me/mailFolders", body: `{"value":[]}`})
	f.on(route{method: "GET", path: "/users/James@tronai.example/messages", body: `{"value":[]}`})
	f.on(route{method: "GET", path: "/me/messages/nope", status: 404, fixture: "error_not_found.json"})
	p, _ := f.provider(t)
	for _, ok := range []string{"/me/mailFolders", "/users/James@tronai.example/messages"} {
		if r, err := p.Raw(ctx, provider.RawRequest{Method: "GET", Path: ok}); err != nil || r.Status != 200 {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"/users/ceo@tronai.example/messages", "/groups", "/me/../users/x", "/me/%2e%2e/users/x", "/meeting"} {
		if _, err := p.Raw(ctx, provider.RawRequest{Method: "GET", Path: bad}); errs.ExitCode(err) != errs.ExitPolicy && errs.ExitCode(err) != errs.ExitUsage {
			t.Errorf("%s allowed: %v", bad, err)
		}
	}
	r, err := p.Raw(ctx, provider.RawRequest{Method: "GET", Path: "/me/messages/nope"})
	if err != nil || r.Status != 404 {
		t.Fatalf("404 passthrough: %v %+v", err, r)
	}
}

func TestDedupeCache(t *testing.T) {
	raw, err := readFixture("msal_cache_dupes.json")
	if err != nil {
		t.Fatal(err)
	}
	out, err := dedupeCache(raw, "uid.utid")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Account      map[string]json.RawMessage
		RefreshToken map[string]struct {
			Secret string `json:"secret"`
		}
		AppMetadata map[string]json.RawMessage
	}
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Account) != 1 || len(c.RefreshToken) != 1 || len(c.AppMetadata) != 1 {
		t.Fatalf("after dedupe: %s", out)
	}
	for _, rt := range c.RefreshToken {
		if rt.Secret != "rt-family" {
			t.Fatalf("kept %s, want the family refresh token", rt.Secret)
		}
	}
	// Without a home id (during login) other accounts are kept but RTs still deduped.
	out, _ = dedupeCache(raw, "")
	_ = json.Unmarshal(out, &c)
	if len(c.Account) != 2 || len(c.RefreshToken) != 2 {
		t.Fatalf("no-homeID dedupe: %s", out)
	}
}

func TestScopesByMode(t *testing.T) {
	cases := map[string]string{
		"read":  "User.Read Mail.Read offline_access",
		"draft": "User.Read Mail.Read offline_access Mail.ReadWrite",
		"send":  "User.Read Mail.Read offline_access Mail.ReadWrite Mail.Send",
	}
	for mode, want := range cases {
		m := mustMode(mode)
		if got := strings.Join(Scopes(m), " "); got != want {
			t.Errorf("%s: %s", mode, got)
		}
		for _, s := range tokenScopes(m) {
			if s == "offline_access" {
				t.Errorf("%s: offline_access in silent scopes", mode)
			}
		}
	}
	if authority("") != "https://login.microsoftonline.com/organizations" {
		t.Fatal(authority(""))
	}
}
