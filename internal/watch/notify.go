package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// MaxPayloadMessages is how many messages one notification lists; Count
// still gives the real number.
const MaxPayloadMessages = 20

// Payload is the body of one notification. It covers one account.
type Payload struct {
	Source   string `json:"source"`
	Account  string `json:"account"`
	Count    int    `json:"count"`
	Messages []Item `json:"messages"`
}

func newPayload(account string, items []Item) Payload {
	p := Payload{Source: "email-watch", Account: account, Count: len(items), Messages: items}
	if len(items) > MaxPayloadMessages {
		p.Messages = items[:MaxPayloadMessages]
	}
	if p.Messages == nil {
		p.Messages = []Item{}
	}
	return p
}

// Notifier delivers a batch. An error means the batch is lost.
type Notifier interface {
	Notify(ctx context.Context, p Payload) error
}

// DefaultRetries are the waits before the 2nd, 3rd and 4th attempt.
var DefaultRetries = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// HTTPNotifier POSTs the payload as JSON, with the token in X-Gitlab-Token
// (the header the Hermes webhook adapter checks).
type HTTPNotifier struct {
	URL     string
	Token   string
	Client  *http.Client    // nil: a client with a 30 s timeout
	Retries []time.Duration // nil: DefaultRetries
	Log     *slog.Logger
}

func (n *HTTPNotifier) Notify(ctx context.Context, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	retries := n.Retries
	if retries == nil {
		retries = DefaultRetries
	}
	for attempt := 0; ; attempt++ {
		err = n.post(ctx, client, body)
		if err == nil || ctx.Err() != nil || attempt >= len(retries) {
			return err
		}
		if n.Log != nil {
			n.Log.Warn("notification failed, retrying", "account", p.Account, "event", "notify_retry",
				"attempt", attempt+1, "retry_in", retries[attempt].String(), "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retries[attempt]):
		}
	}
}

func (n *HTTPNotifier) post(ctx context.Context, client *http.Client, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("X-Gitlab-Token", n.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// WriterNotifier prints each payload as one JSON line (--dry-run).
type WriterNotifier struct {
	W  io.Writer
	mu sync.Mutex
}

func (n *WriterNotifier) Notify(_ context.Context, p Payload) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	enc := json.NewEncoder(n.W)
	enc.SetEscapeHTML(false)
	return enc.Encode(p)
}
