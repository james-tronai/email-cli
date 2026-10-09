package watch

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

// maxNotified is how many notified ids an account remembers.
const maxNotified = 1000

// Item is the metadata of one new message, as the notification carries
// it. Bodies are never read: the agent fetches one with `email get`.
type Item struct {
	ID             string           `json:"id"`
	ThreadID       string           `json:"thread_id,omitempty"`
	From           provider.Address `json:"from"`
	Subject        string           `json:"subject"`
	Date           time.Time        `json:"date"`
	WebLink        string           `json:"web_link,omitempty"`
	HasAttachments bool             `json:"has_attachments"`
	IsRead         bool             `json:"is_read"`
}

func itemOf(m *provider.Message) Item {
	return Item{
		ID:             m.ID,
		ThreadID:       m.ThreadID,
		From:           m.From,
		Subject:        m.Subject,
		Date:           m.Date.UTC(),
		WebLink:        m.WebLink,
		HasAttachments: m.Flags.HasAttachments,
		IsRead:         !m.Flags.Unread,
	}
}

type pendingItem struct {
	Item
	QueuedAt time.Time `json:"queued_at"`
}

// fileState is <state-dir>/<alias>.json: the provider cursor, the ids
// already notified and the batch waiting for its debounce window. It is
// separate from `email sync`'s state; neither moves the other.
type fileState struct {
	Version  int           `json:"version"`
	Provider string        `json:"provider"`
	Cursor   string        `json:"cursor,omitempty"`
	Notified []string      `json:"notified"`
	Pending  []pendingItem `json:"pending,omitempty"`
}

func statePath(dir, alias string) string { return filepath.Join(dir, alias+".json") }

// loadState returns an empty state for a missing file. A state for another
// provider (the alias was logged in again elsewhere) keeps the notified
// ids but drops the cursor.
func loadState(path, providerName string) (*fileState, error) {
	st := &fileState{Version: 1, Provider: providerName, Notified: []string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	var got fileState
	if err := json.Unmarshal(b, &got); err != nil {
		return st, err
	}
	if got.Provider != providerName {
		got.Cursor = ""
	}
	got.Version, got.Provider = 1, providerName
	if got.Notified == nil {
		got.Notified = []string{}
	}
	return &got, nil
}

func saveState(path string, st *fileState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// rememberNotified appends ids, keeping the newest maxNotified.
func (st *fileState) rememberNotified(ids []string) {
	st.Notified = append(st.Notified, ids...)
	if n := len(st.Notified) - maxNotified; n > 0 {
		st.Notified = append([]string(nil), st.Notified[n:]...)
	}
}

// filtered says why the [watch] policy drops a message ("" = keep).
func filtered(f policy.WatchSection, m *provider.Message) string {
	if f.OnlyUnread && !m.Flags.Unread {
		return "already read"
	}
	from := strings.ToLower(m.From.Email)
	for _, s := range f.IgnoreSenders {
		if strings.Contains(from, s) {
			return "ignore_senders " + s
		}
	}
	subj := strings.ToLower(m.Subject)
	for _, s := range f.IgnoreSubjects {
		if strings.Contains(subj, s) {
			return "ignore_subjects " + s
		}
	}
	return ""
}
