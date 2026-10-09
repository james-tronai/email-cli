// Package audit appends one JSON line per write operation (and per policy
// denial) to audit.jsonl in the state directory.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	Time       time.Time `json:"ts"`
	Account    string    `json:"account"`
	Provider   string    `json:"provider"`
	Op         string    `json:"op"`
	Target     string    `json:"target,omitempty"`
	Recipients []string  `json:"recipients,omitempty"`
	Decision   string    `json:"decision"` // allow | deny
	Reason     string    `json:"reason,omitempty"`
	Result     string    `json:"result,omitempty"` // ok | error | dry_run
	ResultID   string    `json:"result_id,omitempty"`
	Count      int       `json:"count,omitempty"` // watch.notify: new messages in the batch
	IDs        []string  `json:"ids,omitempty"`   // watch.notify: their ids
	Error      string    `json:"error,omitempty"`
}

type Logger struct {
	Path string
	mu   sync.Mutex
}

// DefaultStateDir is $EMAIL_STATE_DIR, else $XDG_STATE_HOME/email, else
// ~/.local/state/email.
func DefaultStateDir() string {
	if d := os.Getenv("EMAIL_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "email")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "email")
}

func New(stateDir string) *Logger {
	return &Logger{Path: filepath.Join(stateDir, "audit.jsonl")}
}

func (l *Logger) Log(e Entry) error {
	if l == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
