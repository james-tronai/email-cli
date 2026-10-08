// Package msgraph implements the provider interfaces on Microsoft Graph
// (Microsoft 365 / Exchange Online) with delegated, device-code auth.
package msgraph

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

type Config struct {
	Alias           string
	Username        string // locked UPN; `api` may also address /users/<this>
	BaseURL         string // default DefaultBaseURL
	HTTP            *http.Client
	Tokens          TokenSource
	UserAgent       string
	FolderCachePath string // "" = in memory only
}

type Provider struct {
	c               *client
	alias           string
	username        string
	folders         *folderCache
	folderCachePath string
}

func New(cfg Config) (*Provider, error) {
	c, err := newClient(firstNonEmpty(cfg.BaseURL, DefaultBaseURL), cfg.HTTP, cfg.Tokens, cfg.UserAgent, cfg.Alias)
	if err != nil {
		return nil, err
	}
	return &Provider{c: c, alias: cfg.Alias, username: cfg.Username, folderCachePath: cfg.FolderCachePath}, nil
}

func (p *Provider) Name() string { return Name }

// query encodes OData params with %20 for spaces (Graph does not treat '+'
// as a space everywhere).
func query(v url.Values) string {
	return strings.ReplaceAll(v.Encode(), "+", "%20")
}

func odataString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func limitOr(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 1000)
}

func msgPath(id string) string { return "/me/messages/" + url.PathEscape(id) }

// fetchPage GETs a message collection URL. knownLabel, when set, is the label
// of every message (we listed one folder); otherwise each message's
// parentFolderId is resolved.
func (p *Provider) fetchPage(ctx context.Context, pathOrURL, knownLabel string, hdr map[string]string) (*provider.Page, error) {
	var gp gPage
	if err := p.c.doJSON(ctx, http.MethodGet, pathOrURL, nil, &gp, hdr); err != nil {
		return nil, err
	}
	pg := &provider.Page{Messages: make([]provider.Message, 0, len(gp.Value)), NextCursor: gp.NextLink}
	for i := range gp.Value {
		label := knownLabel
		if label == "" {
			label = p.labelForFolder(ctx, gp.Value[i].ParentFolderID)
		}
		pg.Messages = append(pg.Messages, p.convert(&gp.Value[i], label))
	}
	return pg, nil
}

// List lists a folder newest first. A label that is not a folder is treated
// as an Outlook category.
//
// Graph rejects $filter + $orderby ("InefficientFilter") unless the $orderby
// property also leads the $filter, so receivedDateTime always comes first.
func (p *Provider) List(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	label := firstNonEmpty(o.Label, provider.LabelInbox)
	if o.Cursor != "" {
		known := ""
		if isReserved(label) {
			known = strings.ToLower(label)
		}
		return p.fetchPage(ctx, o.Cursor, known, nil)
	}
	ref, isFolder, err := p.folderRef(ctx, label)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("$select", listSelect)
	q.Set("$top", strconv.Itoa(limitOr(o.Limit, 20)))
	q.Set("$orderby", "receivedDateTime desc")

	var filters []string
	if o.Unread || !o.Since.IsZero() || !isFolder {
		since := "1900-01-01T00:00:00Z"
		if !o.Since.IsZero() {
			since = o.Since.UTC().Format("2006-01-02T15:04:05Z")
		}
		filters = append(filters, "receivedDateTime ge "+since)
	}
	if o.Unread {
		filters = append(filters, "isRead eq false")
	}
	path, known := "/me/mailFolders/"+ref+"/messages", label
	if isReserved(label) {
		known = strings.ToLower(label)
	}
	if !isFolder {
		filters = append(filters, "categories/any(c:c eq "+odataString(label)+")")
		path, known = "/me/messages", ""
	}
	if len(filters) > 0 {
		q.Set("$filter", strings.Join(filters, " and "))
	}
	return p.fetchPage(ctx, path+"?"+query(q), known, nil)
}

// Search uses $search (KQL). Graph forbids combining it with $filter or
// $orderby; results come back by relevance/date as Graph decides.
func (p *Provider) Search(ctx context.Context, o provider.SearchOptions) (*provider.Page, error) {
	if o.Cursor != "" {
		return p.fetchPage(ctx, o.Cursor, "", nil)
	}
	if strings.TrimSpace(o.Query) == "" {
		return nil, errs.Usage("search query is empty")
	}
	q := url.Values{}
	q.Set("$search", `"`+strings.ReplaceAll(o.Query, `"`, `\"`)+`"`)
	q.Set("$select", listSelect)
	q.Set("$top", strconv.Itoa(limitOr(o.Limit, 20)))
	return p.fetchPage(ctx, "/me/messages?"+query(q), "", nil)
}

// Get fetches one message with its body in the stored content type. HTML is
// returned as HTML on purpose: the service derives text from it locally so
// hidden text is stripped (Graph's own text conversion keeps hidden text).
func (p *Provider) Get(ctx context.Context, id string, o provider.GetOptions) (*provider.Message, error) {
	sel := getSelect
	if o.Headers {
		sel += ",internetMessageHeaders"
	}
	var g gMessage
	if err := p.c.doJSON(ctx, http.MethodGet, msgPath(id)+"?$select="+sel, nil, &g, nil); err != nil {
		return nil, err
	}
	m := p.convert(&g, p.labelForFolder(ctx, g.ParentFolderID))
	if !o.Headers {
		m.Headers = nil
	}
	return &m, nil
}

const maxThreadMessages = 200

// Thread returns a conversation oldest first. Graph rejects $orderby with a
// conversationId filter, so sorting is local.
func (p *Provider) Thread(ctx context.Context, threadID string, limit int) ([]provider.Message, error) {
	q := url.Values{}
	q.Set("$filter", "conversationId eq "+odataString(threadID))
	q.Set("$select", getSelect)
	q.Set("$top", "50")
	next := "/me/messages?" + query(q)
	var out []provider.Message
	for next != "" && len(out) < maxThreadMessages {
		var gp gPage
		if err := p.c.doJSON(ctx, http.MethodGet, next, nil, &gp, nil); err != nil {
			return nil, err
		}
		for i := range gp.Value {
			m := p.convert(&gp.Value[i], p.labelForFolder(ctx, gp.Value[i].ParentFolderID))
			m.Headers = nil
			out = append(out, m)
		}
		next = gp.NextLink
	}
	if len(out) == 0 {
		return nil, errs.Provider("ErrorItemNotFound", fmt.Errorf("thread %s not found", threadID))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

type gAttachment struct {
	ODataType    string `json:"@odata.type"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	Size         int64  `json:"size"`
	IsInline     bool   `json:"isInline"`
	ContentBytes string `json:"contentBytes"`
}

func (a *gAttachment) meta() provider.AttachmentMeta {
	return provider.AttachmentMeta{ID: a.ID, Name: a.Name, Size: a.Size, MIME: a.ContentType, Inline: a.IsInline}
}

func (p *Provider) ListAttachments(ctx context.Context, id string) ([]provider.AttachmentMeta, error) {
	var res struct {
		Value []gAttachment `json:"value"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, msgPath(id)+"/attachments?$select=id,name,contentType,size,isInline", nil, &res, nil); err != nil {
		return nil, err
	}
	out := make([]provider.AttachmentMeta, len(res.Value))
	for i := range res.Value {
		out[i] = res.Value[i].meta()
	}
	return out, nil
}

func (p *Provider) GetAttachment(ctx context.Context, id, aid string) (*provider.Attachment, error) {
	var a gAttachment
	if err := p.c.doJSON(ctx, http.MethodGet, msgPath(id)+"/attachments/"+url.PathEscape(aid), nil, &a, nil); err != nil {
		return nil, err
	}
	if a.ODataType != "#microsoft.graph.fileAttachment" {
		return nil, errs.Unsupported("attachment %s is %s; only file attachments can be downloaded", aid, a.ODataType)
	}
	data, err := base64.StdEncoding.DecodeString(a.ContentBytes)
	if err != nil {
		return nil, errs.Provider("invalidResponse", fmt.Errorf("attachment %s: %w", aid, err))
	}
	return &provider.Attachment{AttachmentMeta: a.meta(), Data: data}, nil
}
