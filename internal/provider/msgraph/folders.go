package msgraph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/james-tronai/email-cli/internal/provider"
)

// wellKnown maps reserved labels to Graph well-known folder names, which can
// be used directly in paths (/me/mailFolders/sentitems/messages).
var wellKnown = map[string]string{
	provider.LabelInbox:   "inbox",
	provider.LabelArchive: "archive",
	provider.LabelSent:    "sentitems",
	provider.LabelDrafts:  "drafts",
	provider.LabelTrash:   "deleteditems",
	provider.LabelSpam:    "junkemail",
}

// folderCache maps folder id → unified label. Folder ids are stable, so the
// map is persisted per account to avoid six lookups per invocation.
type folderCache struct {
	ReservedLoaded bool              `json:"reserved_loaded"`
	Labels         map[string]string `json:"labels"`
}

func (p *Provider) loadFolders() {
	if p.folders != nil {
		return
	}
	p.folders = &folderCache{Labels: map[string]string{}}
	if p.folderCachePath == "" {
		return
	}
	if b, err := os.ReadFile(p.folderCachePath); err == nil {
		var fc folderCache
		if json.Unmarshal(b, &fc) == nil && fc.Labels != nil {
			p.folders = &fc
		}
	}
}

func (p *Provider) saveFolders() {
	if p.folderCachePath == "" {
		return
	}
	b, err := json.Marshal(p.folders)
	if err != nil {
		return
	}
	tmp := p.folderCachePath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p.folderCachePath)
	}
}

type gFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// labelForFolder returns the unified label for a parentFolderId.
func (p *Provider) labelForFolder(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	p.loadFolders()
	if l, ok := p.folders.Labels[id]; ok {
		return l
	}
	changed := false
	if !p.folders.ReservedLoaded {
		for label, wk := range wellKnown {
			var f gFolder
			if err := p.c.doJSON(ctx, http.MethodGet, "/me/mailFolders/"+wk+"?$select=id,displayName", nil, &f, nil); err == nil && f.ID != "" {
				p.folders.Labels[f.ID] = label
			}
		}
		p.folders.ReservedLoaded, changed = true, true
	}
	if _, ok := p.folders.Labels[id]; !ok {
		var f gFolder
		if err := p.c.doJSON(ctx, http.MethodGet, "/me/mailFolders/"+url.PathEscape(id)+"?$select=id,displayName", nil, &f, nil); err == nil {
			p.folders.Labels[id] = f.DisplayName
			changed = true
		}
	}
	if changed {
		p.saveFolders()
	}
	return p.folders.Labels[id]
}

// folderRef resolves a label to a path segment usable in /me/mailFolders/{ref}.
// ok is false when no folder has that name (the label may be a category).
func (p *Provider) folderRef(ctx context.Context, label string) (ref string, ok bool, err error) {
	if wk, found := wellKnown[strings.ToLower(label)]; found {
		return wk, true, nil
	}
	filter := url.QueryEscape("displayName eq '" + strings.ReplaceAll(label, "'", "''") + "'")
	for _, base := range []string{"/me/mailFolders", "/me/mailFolders/inbox/childFolders"} {
		var res struct {
			Value []gFolder `json:"value"`
		}
		if err := p.c.doJSON(ctx, http.MethodGet, base+"?$filter="+filter+"&$select=id,displayName", nil, &res, nil); err != nil {
			return "", false, err
		}
		if len(res.Value) > 0 {
			p.loadFolders()
			p.folders.Labels[res.Value[0].ID] = res.Value[0].DisplayName
			return url.PathEscape(res.Value[0].ID), true, nil
		}
	}
	return "", false, nil
}

func isReserved(label string) bool {
	_, ok := wellKnown[strings.ToLower(label)]
	return ok
}
