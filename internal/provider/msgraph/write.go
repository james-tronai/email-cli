package msgraph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

// MaxSimpleAttachment is the largest attachment sent in one POST. Larger
// files need an upload session (planned for M4).
const MaxSimpleAttachment = 3 << 20

func checkAttachments(files []provider.AttachmentFile) error {
	for _, f := range files {
		if len(f.Data) >= MaxSimpleAttachment {
			return errs.Unsupported("attachment %s is %d bytes; attachments of 3 MB or more are not supported yet", f.Name, len(f.Data))
		}
	}
	return nil
}

func (p *Provider) addAttachments(ctx context.Context, draftID string, files []provider.AttachmentFile) error {
	for _, f := range files {
		body := map[string]any{
			"@odata.type":  "#microsoft.graph.fileAttachment",
			"name":         f.Name,
			"contentType":  firstNonEmpty(f.MIME, "application/octet-stream"),
			"contentBytes": base64.StdEncoding.EncodeToString(f.Data),
		}
		if err := p.c.doJSON(ctx, http.MethodPost, msgPath(draftID)+"/attachments", body, nil, nil); err != nil {
			return fmt.Errorf("draft %s was created but attaching %s failed: %w", draftID, f.Name, err)
		}
	}
	return nil
}

func (p *Provider) draftResult(ctx context.Context, g *gMessage, attached bool) (*provider.Message, error) {
	if attached {
		// Re-read so hasAttachments reflects the uploads.
		return p.Get(ctx, g.ID, provider.GetOptions{})
	}
	m := p.convert(g, provider.LabelDrafts)
	return &m, nil
}

func (p *Provider) CreateDraft(ctx context.Context, in provider.DraftInput) (*provider.Message, error) {
	if err := checkAttachments(in.Attachments); err != nil {
		return nil, err
	}
	body := map[string]any{
		"subject":       in.Subject,
		"body":          gBody{ContentType: "Text", Content: in.Body},
		"toRecipients":  toRecipients(in.To),
		"ccRecipients":  toRecipients(in.Cc),
		"bccRecipients": toRecipients(in.Bcc),
	}
	var g gMessage
	if err := p.c.doJSON(ctx, http.MethodPost, "/me/messages", body, &g, nil); err != nil {
		return nil, err
	}
	if err := p.addAttachments(ctx, g.ID, in.Attachments); err != nil {
		return nil, err
	}
	return p.draftResult(ctx, &g, len(in.Attachments) > 0)
}

// CreateReplyDraft uses createReply/createReplyAll so Exchange sets
// In-Reply-To/References and quotes the original. The agent's text goes in
// `comment`, which Graph places above the quote; a separate PATCH of the
// body would replace the quote and could leave an empty draft behind if it
// failed.
func (p *Provider) CreateReplyDraft(ctx context.Context, in provider.ReplyInput) (*provider.Message, error) {
	if err := checkAttachments(in.Attachments); err != nil {
		return nil, err
	}
	action := "/createReply"
	if in.All {
		action = "/createReplyAll"
	}
	var g gMessage
	if err := p.c.doJSON(ctx, http.MethodPost, msgPath(in.MessageID)+action, map[string]any{"comment": in.Body}, &g, nil); err != nil {
		return nil, err
	}
	if err := p.addAttachments(ctx, g.ID, in.Attachments); err != nil {
		return nil, err
	}
	return p.draftResult(ctx, &g, len(in.Attachments) > 0)
}

func (p *Provider) CreateForwardDraft(ctx context.Context, in provider.ForwardInput) (*provider.Message, error) {
	var g gMessage
	body := map[string]any{"comment": in.Body, "toRecipients": toRecipients(in.To)}
	if err := p.c.doJSON(ctx, http.MethodPost, msgPath(in.MessageID)+"/createForward", body, &g, nil); err != nil {
		return nil, err
	}
	return p.draftResult(ctx, &g, false)
}

// requireDraft refuses to modify anything that is not a draft.
func (p *Provider) requireDraft(ctx context.Context, id string) error {
	var g struct {
		IsDraft bool `json:"isDraft"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, msgPath(id)+"?$select=isDraft", nil, &g, nil); err != nil {
		return err
	}
	if !g.IsDraft {
		return errs.Usage("%s is not a draft", id)
	}
	return nil
}

// UpdateDraft PATCHes the given fields. A new body replaces the whole body,
// including any quoted text from a reply.
func (p *Provider) UpdateDraft(ctx context.Context, id string, u provider.DraftUpdate) (*provider.Message, error) {
	if err := checkAttachments(u.Attachments); err != nil {
		return nil, err
	}
	if err := p.requireDraft(ctx, id); err != nil {
		return nil, err
	}
	patch := map[string]any{}
	if u.To != nil {
		patch["toRecipients"] = toRecipients(*u.To)
	}
	if u.Cc != nil {
		patch["ccRecipients"] = toRecipients(*u.Cc)
	}
	if u.Bcc != nil {
		patch["bccRecipients"] = toRecipients(*u.Bcc)
	}
	if u.Subject != nil {
		patch["subject"] = *u.Subject
	}
	if u.Body != nil {
		patch["body"] = gBody{ContentType: "Text", Content: *u.Body}
	}
	var g gMessage
	if len(patch) > 0 {
		if err := p.c.doJSON(ctx, http.MethodPatch, msgPath(id), patch, &g, nil); err != nil {
			return nil, err
		}
	} else {
		g.ID = id
	}
	if err := p.addAttachments(ctx, id, u.Attachments); err != nil {
		return nil, err
	}
	return p.draftResult(ctx, &g, len(u.Attachments) > 0 || len(patch) == 0)
}

func (p *Provider) ListDrafts(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	o.Label = provider.LabelDrafts
	return p.List(ctx, o)
}

func (p *Provider) DeleteDraft(ctx context.Context, id string) error {
	if err := p.requireDraft(ctx, id); err != nil {
		return err
	}
	_, err := p.c.do(ctx, http.MethodDelete, msgPath(id), nil, nil)
	return err
}

// Move is a real folder move; Graph returns the message with a new id.
func (p *Provider) Move(ctx context.Context, id, dest string) (*provider.Message, error) {
	ref, ok, err := p.folderRef(ctx, dest)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.Usage("no folder named %q (use `label --add` for categories)", dest)
	}
	destID, _ := url.PathUnescape(ref)
	var g gMessage
	if err := p.c.doJSON(ctx, http.MethodPost, msgPath(id)+"/move", map[string]string{"destinationId": destID}, &g, nil); err != nil {
		return nil, err
	}
	label := dest
	if isReserved(dest) {
		label = strings.ToLower(dest)
	}
	m := p.convert(&g, label)
	return &m, nil
}

// Relabel edits Outlook categories. Reserved labels are folders: use move.
func (p *Provider) Relabel(ctx context.Context, id string, add, remove []string) (*provider.Message, error) {
	for _, l := range append(append([]string{}, add...), remove...) {
		if isReserved(l) {
			return nil, errs.Usage("%q is a folder on Microsoft 365; use `email move --to %s`", l, l)
		}
	}
	var cur struct {
		Categories []string `json:"categories"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, msgPath(id)+"?$select=categories", nil, &cur, nil); err != nil {
		return nil, err
	}
	cats := slices.DeleteFunc(cur.Categories, func(c string) bool {
		return slices.ContainsFunc(remove, func(r string) bool { return strings.EqualFold(r, c) })
	})
	for _, a := range add {
		if !slices.ContainsFunc(cats, func(c string) bool { return strings.EqualFold(c, a) }) {
			cats = append(cats, a)
		}
	}
	if cats == nil {
		cats = []string{}
	}
	return p.patchMessage(ctx, id, map[string]any{"categories": cats})
}

func (p *Provider) Mark(ctx context.Context, id string, o provider.MarkOptions) (*provider.Message, error) {
	patch := map[string]any{}
	if o.Read != nil {
		patch["isRead"] = *o.Read
	}
	if o.Flagged != nil {
		status := "notFlagged"
		if *o.Flagged {
			status = "flagged"
		}
		patch["flag"] = map[string]string{"flagStatus": status}
	}
	return p.patchMessage(ctx, id, patch)
}

func (p *Provider) patchMessage(ctx context.Context, id string, patch map[string]any) (*provider.Message, error) {
	var g gMessage
	if err := p.c.doJSON(ctx, http.MethodPatch, msgPath(id), patch, &g, nil); err != nil {
		return nil, err
	}
	m := p.convert(&g, p.labelForFolder(ctx, g.ParentFolderID))
	return &m, nil
}

// ---- sync (delta) ----

type syncState struct {
	DeltaLink string    `json:"delta"`
	Watermark time.Time `json:"watermark"`
	SeenAtMax []string  `json:"seen,omitempty"` // ids received exactly at Watermark
}

const maxDeltaPages = 100

var deltaHeaders = map[string]string{"Prefer": "odata.maxpagesize=50"}

// Sync uses messages/delta on the folder. The first run asks for mail
// received from now on (so the baseline is cheap) and returns nothing.
// Delta also reports read/flag changes on older mail; only messages received
// after the watermark count as new.
func (p *Provider) Sync(ctx context.Context, o provider.SyncOptions) (*provider.SyncResult, error) {
	label := firstNonEmpty(o.Label, provider.LabelInbox)
	var st syncState
	if o.State != "" {
		if err := json.Unmarshal([]byte(o.State), &st); err != nil || st.DeltaLink == "" {
			return nil, errs.Usage("sync state is not a msgraph cursor; delete the state file to start over")
		}
	}
	if st.DeltaLink == "" {
		return p.syncBaseline(ctx, label)
	}

	res := &provider.SyncResult{Messages: []provider.Message{}}
	next, newState := st.DeltaLink, st
	seen := map[string]bool{}
	for _, id := range st.SeenAtMax {
		seen[id] = true
	}
	for pages := 0; next != ""; pages++ {
		if pages >= maxDeltaPages {
			return nil, errs.Provider("tooManyPages", fmt.Errorf("delta returned more than %d pages", maxDeltaPages))
		}
		var gp gPage
		if err := p.c.doJSON(ctx, http.MethodGet, next, nil, &gp, deltaHeaders); err != nil {
			if isSyncReset(err) {
				out, berr := p.syncBaseline(ctx, label)
				if berr != nil {
					return nil, berr
				}
				return out, nil
			}
			return nil, err
		}
		for i := range gp.Value {
			g := &gp.Value[i]
			if g.Removed != nil || g.IsDraft {
				continue
			}
			t := g.ReceivedDateTime
			if t.Before(st.Watermark) || (t.Equal(st.Watermark) && seen[g.ID]) {
				continue // a change to mail we already reported
			}
			seen[g.ID] = true
			res.Messages = append(res.Messages, p.convert(g, strings.ToLower(label)))
			switch {
			case t.After(newState.Watermark):
				newState.Watermark, newState.SeenAtMax = t, []string{g.ID}
			case t.Equal(newState.Watermark) && !slices.Contains(newState.SeenAtMax, g.ID):
				newState.SeenAtMax = append(newState.SeenAtMax, g.ID)
			}
		}
		if gp.DeltaLink != "" {
			newState.DeltaLink = gp.DeltaLink
		}
		next = gp.NextLink
	}
	slices.SortStableFunc(res.Messages, func(a, b provider.Message) int { return a.Date.Compare(b.Date) })
	b, _ := json.Marshal(newState)
	res.State = string(b)
	return res, nil
}

func (p *Provider) syncBaseline(ctx context.Context, label string) (*provider.SyncResult, error) {
	ref, ok, err := p.folderRef(ctx, label)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.Usage("sync needs a folder; %q is not one", label)
	}
	now := time.Now().UTC().Truncate(time.Second)
	q := url.Values{}
	q.Set("$select", listSelect)
	q.Set("$filter", "receivedDateTime ge "+now.Format("2006-01-02T15:04:05Z"))
	next := "/me/mailFolders/" + ref + "/messages/delta?" + query(q)
	st := syncState{Watermark: now}
	for pages := 0; next != "" && st.DeltaLink == ""; pages++ {
		if pages >= maxDeltaPages {
			return nil, errs.Provider("tooManyPages", fmt.Errorf("delta baseline exceeded %d pages", maxDeltaPages))
		}
		var gp gPage
		if err := p.c.doJSON(ctx, http.MethodGet, next, nil, &gp, deltaHeaders); err != nil {
			return nil, err
		}
		for _, g := range gp.Value {
			if g.ReceivedDateTime.After(st.Watermark) {
				st.Watermark = g.ReceivedDateTime
			}
		}
		st.DeltaLink, next = gp.DeltaLink, gp.NextLink
	}
	if st.DeltaLink == "" {
		return nil, errs.Provider("noDeltaLink", errors.New("delta query ended without a deltaLink"))
	}
	b, _ := json.Marshal(st)
	return &provider.SyncResult{Messages: []provider.Message{}, State: string(b), Initialized: true}, nil
}

func isSyncReset(err error) bool {
	var e *errs.Error
	if !errors.As(err, &e) {
		return false
	}
	c := strings.ToLower(e.ProviderCode)
	return c == "410" || strings.Contains(c, "syncstate") || strings.Contains(c, "resyncrequired")
}

// ---- raw API ----

// Raw allows only paths under /me or /users/<locked account>; the policy
// layer has already checked the method against the mode.
func (p *Provider) Raw(ctx context.Context, r provider.RawRequest) (*provider.RawResponse, error) {
	if err := p.checkRawPath(r.Path); err != nil {
		return nil, err
	}
	resp, err := p.c.do(ctx, strings.ToUpper(r.Method), r.Path, r.Body, nil)
	var e *errs.Error
	if errors.As(err, &e) && e.Exit != errs.ExitProvider {
		return nil, err // auth, timeout, usage
	}
	if resp == nil {
		return nil, err
	}
	return &provider.RawResponse{Status: resp.Status, Body: resp.Body}, nil
}

func (p *Provider) checkRawPath(path string) error {
	pathOnly := path
	if i := strings.IndexAny(pathOnly, "?#"); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	dec, err := url.PathUnescape(pathOnly)
	if err != nil {
		return errs.Usage("invalid path %q", path)
	}
	for _, seg := range strings.Split(dec, "/") {
		if seg == ".." || seg == "." {
			return errs.Usage("path must not contain . or .. segments")
		}
	}
	low := strings.ToLower(dec)
	if low == "/me" || strings.HasPrefix(low, "/me/") {
		return nil
	}
	if p.username != "" {
		u := "/users/" + strings.ToLower(p.username)
		if low == u || strings.HasPrefix(low, u+"/") {
			return nil
		}
	}
	return errs.Policy("api paths must start with /me or /users/%s", p.username)
}
