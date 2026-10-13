package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

// ---- move / label / mark ----

func (p *Provider) modify(ctx context.Context, id string, add, remove []string) (*provider.Message, error) {
	if isDraftID(id) {
		return nil, errs.Usage("%s is a draft id; drafts cannot be moved or labelled", id)
	}
	body := map[string][]string{"addLabelIds": add, "removeLabelIds": remove}
	if add == nil {
		body["addLabelIds"] = []string{}
	}
	if remove == nil {
		body["removeLabelIds"] = []string{}
	}
	if err := p.c.doJSON(ctx, http.MethodPost, userPath+"/messages/"+pathEsc(id)+"/modify", body, nil); err != nil {
		return nil, err
	}
	return p.reread(ctx, id)
}

func (p *Provider) reread(ctx context.Context, id string) (*provider.Message, error) {
	ms, err := p.getMetadata(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, errs.NotFound("notFound", fmt.Errorf("message %s not found", id))
	}
	return &ms[0], nil
}

// ensureLabel returns the id of a user label, creating it when missing.
func (p *Provider) ensureLabel(ctx context.Context, name string) (string, error) {
	id, ok, err := p.labelID(ctx, name)
	if err != nil || ok {
		return id, err
	}
	var l gLabel
	body := map[string]string{"name": name, "labelListVisibility": "labelShow", "messageListVisibility": "show"}
	if err := p.c.doJSON(ctx, http.MethodPost, userPath+"/labels", body, &l); err != nil {
		return "", err
	}
	if _, err := p.loadLabels(ctx, true); err != nil {
		return "", err
	}
	return l.ID, nil
}

// Move changes labels the way Gmail's "Move to" does: archive removes
// INBOX; spam and user labels replace INBOX; trash uses messages.trash.
func (p *Provider) Move(ctx context.Context, id, dest string) (*provider.Message, error) {
	switch strings.ToLower(dest) {
	case provider.LabelArchive:
		return p.modify(ctx, id, nil, []string{"INBOX", "SPAM"})
	case provider.LabelInbox:
		return p.modify(ctx, id, []string{"INBOX"}, []string{"SPAM"})
	case provider.LabelSpam:
		return p.modify(ctx, id, []string{"SPAM"}, []string{"INBOX"})
	case provider.LabelTrash:
		if err := p.c.doJSON(ctx, http.MethodPost, userPath+"/messages/"+pathEsc(id)+"/trash", nil, nil); err != nil {
			return nil, err
		}
		return p.reread(ctx, id)
	case provider.LabelSent, provider.LabelDrafts:
		return nil, errs.Usage("messages cannot be moved to %s on Gmail", dest)
	}
	lid, err := p.ensureLabel(ctx, dest)
	if err != nil {
		return nil, err
	}
	return p.modify(ctx, id, []string{lid}, []string{"INBOX"})
}

// Relabel adds and removes labels. User labels are created on first use.
func (p *Provider) Relabel(ctx context.Context, id string, add, remove []string) (*provider.Message, error) {
	sys := func(l string) (string, bool, error) {
		switch strings.ToLower(l) {
		case provider.LabelInbox, provider.LabelSpam:
			return reservedToSystem[strings.ToLower(l)], true, nil
		case provider.LabelArchive:
			return "", true, errs.Usage("archive is not a label; use `email move --to archive`")
		case provider.LabelTrash:
			return "", true, errs.Usage("use `email move --to trash`")
		case provider.LabelSent, provider.LabelDrafts:
			return "", true, errs.Usage("Gmail does not allow changing the %s label", l)
		}
		return "", false, nil
	}
	var addIDs, rmIDs []string
	for _, l := range add {
		id, ok, err := sys(l)
		if err != nil {
			return nil, err
		}
		if !ok {
			if id, err = p.ensureLabel(ctx, l); err != nil {
				return nil, err
			}
		}
		addIDs = append(addIDs, id)
	}
	for _, l := range remove {
		id, ok, err := sys(l)
		if err != nil {
			return nil, err
		}
		if !ok {
			var found bool
			if id, found, err = p.labelID(ctx, l); err != nil {
				return nil, err
			}
			if !found {
				continue // nothing to remove
			}
		}
		rmIDs = append(rmIDs, id)
	}
	return p.modify(ctx, id, addIDs, rmIDs)
}

func (p *Provider) Mark(ctx context.Context, id string, o provider.MarkOptions) (*provider.Message, error) {
	var add, rm []string
	if o.Read != nil {
		if *o.Read {
			rm = append(rm, "UNREAD")
		} else {
			add = append(add, "UNREAD")
		}
	}
	if o.Flagged != nil {
		if *o.Flagged {
			add = append(add, "STARRED")
		} else {
			rm = append(rm, "STARRED")
		}
	}
	return p.modify(ctx, id, add, rm)
}

// ---- sync (history) ----

type syncState struct {
	HistoryID string `json:"history_id"`
	LabelID   string `json:"label_id"`
}

const maxHistoryPages = 100

// Sync uses history.list from the stored historyId. The first run records
// the mailbox's current historyId and returns nothing. When Gmail no longer
// has the history (404: the id is too old), a new baseline is recorded.
func (p *Provider) Sync(ctx context.Context, o provider.SyncOptions) (*provider.SyncResult, error) {
	label := firstNonEmpty(o.Label, provider.LabelInbox)
	lid, ok, err := p.labelID(ctx, label)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.Usage("no Gmail label named %q", label)
	}
	var st syncState
	if o.State != "" {
		if err := json.Unmarshal([]byte(o.State), &st); err != nil || st.HistoryID == "" {
			return nil, errs.Usage("sync state is not a gmail cursor; delete the state file to start over")
		}
		if st.LabelID != lid {
			return nil, errs.Usage("sync state is for label %s, not %s; use another --state-file", st.LabelID, label)
		}
	}
	if st.HistoryID == "" {
		return p.syncBaseline(ctx, lid)
	}

	var ids []string
	seen := map[string]bool{}
	next := st.HistoryID
	token := ""
	for pages := 0; ; pages++ {
		if pages >= maxHistoryPages {
			return nil, errs.Provider("tooManyPages", fmt.Errorf("history returned more than %d pages", maxHistoryPages))
		}
		v := url.Values{}
		v.Set("startHistoryId", st.HistoryID)
		v.Set("historyTypes", "messageAdded")
		v.Set("labelId", lid)
		v.Set("maxResults", "500")
		if token != "" {
			v.Set("pageToken", token)
		}
		var res struct {
			History []struct {
				MessagesAdded []struct {
					Message struct {
						ID       string   `json:"id"`
						LabelIDs []string `json:"labelIds"`
					} `json:"message"`
				} `json:"messagesAdded"`
			} `json:"history"`
			HistoryID     string `json:"historyId"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/history?"+v.Encode(), nil, &res); err != nil {
			if isNotFound(err) {
				return p.syncBaseline(ctx, lid)
			}
			return nil, err
		}
		for _, h := range res.History {
			for _, ma := range h.MessagesAdded {
				m := ma.Message
				if seen[m.ID] || hasLabel(m.LabelIDs, "DRAFT") || !hasLabel(m.LabelIDs, lid) {
					continue
				}
				seen[m.ID] = true
				ids = append(ids, m.ID)
			}
		}
		if res.HistoryID != "" {
			next = res.HistoryID
		}
		if token = res.NextPageToken; token == "" {
			break
		}
	}
	msgs, err := p.getMetadata(ctx, ids)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(msgs, func(a, b provider.Message) int { return a.Date.Compare(b.Date) })
	b, _ := json.Marshal(syncState{HistoryID: next, LabelID: lid})
	return &provider.SyncResult{Messages: msgs, State: string(b)}, nil
}

func (p *Provider) syncBaseline(ctx context.Context, lid string) (*provider.SyncResult, error) {
	var prof struct {
		HistoryID string `json:"historyId"`
	}
	if err := p.c.doJSON(ctx, http.MethodGet, userPath+"/profile", nil, &prof); err != nil {
		return nil, err
	}
	if prof.HistoryID == "" {
		return nil, errs.Provider("invalidResponse", errors.New("profile returned no historyId"))
	}
	b, _ := json.Marshal(syncState{HistoryID: prof.HistoryID, LabelID: lid})
	return &provider.SyncResult{Messages: []provider.Message{}, State: string(b), Initialized: true}, nil
}

// ---- raw API ----

// Raw allows only the mailbox's own Gmail paths; the policy layer has
// already checked the method (and send-like paths) against the mode.
func (p *Provider) Raw(ctx context.Context, r provider.RawRequest) (*provider.RawResponse, error) {
	if err := p.checkRawPath(r.Path); err != nil {
		return nil, err
	}
	resp, err := p.c.do(ctx, strings.ToUpper(r.Method), r.Path, r.Body, "application/json")
	var e *errs.Error
	if errors.As(err, &e) && e.Exit != errs.ExitProvider {
		return nil, err
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
	users := []string{"me"}
	if p.username != "" {
		users = append(users, strings.ToLower(p.username))
	}
	for _, u := range users {
		for _, pre := range []string{"/gmail/v1/users/", "/upload/gmail/v1/users/"} {
			base := pre + u
			if low == base || strings.HasPrefix(low, base+"/") {
				return nil
			}
		}
	}
	return errs.Policy("api paths must start with /gmail/v1/users/me")
}
