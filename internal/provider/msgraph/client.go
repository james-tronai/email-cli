package msgraph

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
)

const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

// TokenSource returns a valid access token for Graph.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// client is a small Graph REST client: auth header, User-Agent,
// client-request-id, Retry-After backoff on 429/503/504, and error mapping.
type client struct {
	base       *url.URL
	http       *http.Client
	tokens     TokenSource
	userAgent  string
	alias      string // for re-login hints
	maxRetries int
	sleep      func(ctx context.Context, d time.Duration) error
}

func newClient(baseURL string, h *http.Client, ts TokenSource, userAgent, alias string) (*client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	if h == nil {
		h = &http.Client{Timeout: 120 * time.Second}
	}
	return &client{base: u, http: h, tokens: ts, userAgent: userAgent, alias: alias, maxRetries: 3, sleep: sleepCtx}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// resolve turns a path ("/me/messages?…") or an absolute nextLink/deltaLink
// into a URL. Absolute URLs must point at the configured Graph host and
// version so a crafted cursor cannot send the token elsewhere.
func (c *client) resolve(pathOrURL string) (string, error) {
	if strings.HasPrefix(pathOrURL, "/") {
		return c.base.String() + pathOrURL, nil
	}
	u, err := url.Parse(pathOrURL)
	if err != nil || u.Scheme != c.base.Scheme || u.Host != c.base.Host || !strings.HasPrefix(u.Path, c.base.Path+"/") {
		return "", errs.Usage("refusing URL outside %s: %q", c.base, pathOrURL)
	}
	return u.String(), nil
}

type response struct {
	Status    int
	Header    http.Header
	Body      []byte
	RequestID string
}

// do performs one logical request with retries. body may be nil.
func (c *client) do(ctx context.Context, method, pathOrURL string, body []byte, hdr map[string]string) (*response, error) {
	target, err := c.resolve(pathOrURL)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return nil, err
		}
		reqID := newRequestID()
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("client-request-id", reqID)
		req.Header.Set("return-client-request-id", "true")
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, errs.Timeout(ctx.Err())
			}
			return nil, errs.Provider("network", fmt.Errorf("%s %s: %w (client-request-id %s)", method, redactURL(target), err, reqID))
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return nil, errs.Provider("network", err)
		}
		r := &response{Status: resp.StatusCode, Header: resp.Header, Body: b, RequestID: firstNonEmpty(resp.Header.Get("request-id"), reqID)}

		if retryable(r.Status) && attempt < c.maxRetries {
			if err := c.sleep(ctx, retryAfter(r.Header, attempt)); err != nil {
				return nil, errs.Timeout(err)
			}
			continue
		}
		if r.Status >= 400 {
			return r, c.graphError(method, target, r)
		}
		return r, nil
	}
}

// doJSON sends v (if non-nil) as JSON and decodes the response into out (if non-nil).
func (c *client) doJSON(ctx context.Context, method, pathOrURL string, v any, out any, hdr map[string]string) error {
	var body []byte
	if v != nil {
		var err error
		if body, err = json.Marshal(v); err != nil {
			return err
		}
	}
	r, err := c.do(ctx, method, pathOrURL, body, hdr)
	if err != nil {
		return err
	}
	if out != nil && len(r.Body) > 0 {
		if err := json.Unmarshal(r.Body, out); err != nil {
			return errs.Provider("invalidResponse", fmt.Errorf("decode %s: %w (request-id %s)", method, err, r.RequestID))
		}
	}
	return nil
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// retryAfter honours Retry-After (seconds), else 1s, 2s, 4s; capped at 60s.
func retryAfter(h http.Header, attempt int) time.Duration {
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			return min(time.Duration(n)*time.Second, 60*time.Second)
		}
		if t, err := http.ParseTime(s); err == nil {
			return min(max(time.Until(t), 0), 60*time.Second)
		}
	}
	return time.Duration(1<<attempt) * time.Second
}

type graphErrorBody struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		InnerError struct {
			Code string `json:"code"`
		} `json:"innerError"`
	} `json:"error"`
}

func (c *client) graphError(method, target string, r *response) error {
	var ge graphErrorBody
	_ = json.Unmarshal(r.Body, &ge)
	code := ge.Error.Code
	if code == "" {
		code = strconv.Itoa(r.Status)
	}
	msg := ge.Error.Message
	if msg == "" {
		msg = http.StatusText(r.Status)
	}
	if r.Status == http.StatusUnauthorized || code == "InvalidAuthenticationToken" {
		e := errs.Auth("Graph rejected the token (%s: %s, request-id %s); run `email auth login -a %s --provider msgraph`", code, msg, r.RequestID, c.alias)
		e.ProviderCode = code
		return e
	}
	return errs.Provider(code, fmt.Errorf("Graph %s %s: HTTP %d %s: %s (request-id %s)", method, redactURL(target), r.Status, code, msg, r.RequestID))
}

// redactURL drops query strings (they can carry skip/delta tokens).
func redactURL(s string) string {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i] + "?…"
	}
	return s
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
