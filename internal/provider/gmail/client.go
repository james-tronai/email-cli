package gmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
)

const DefaultBaseURL = "https://gmail.googleapis.com"

// TokenSource returns a valid access token for the Gmail API.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// client is a small Gmail REST client: auth header, User-Agent, backoff on
// 429/5xx and rate-limit 403s, and error mapping.
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

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// do performs one logical request with retries. path starts with "/" and is
// relative to the base URL. contentType applies when body is non-nil.
func (c *client) do(ctx context.Context, method, path string, body []byte, contentType string) (*response, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errs.Usage("invalid Gmail path %q", path)
	}
	target := c.base.String() + path
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
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, errs.Timeout(ctx.Err())
			}
			return nil, errs.Provider("network", fmt.Errorf("%s %s: %w", method, redactURL(target), err))
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return nil, errs.Provider("network", err)
		}
		r := &response{Status: resp.StatusCode, Header: resp.Header, Body: b}
		if attempt < c.maxRetries && retryable(r) {
			if err := c.sleep(ctx, retryAfter(r.Header, attempt)); err != nil {
				return nil, errs.Timeout(err)
			}
			continue
		}
		if r.Status >= 400 {
			return r, c.apiError(method, target, r)
		}
		return r, nil
	}
}

// doJSON sends v (if non-nil) as JSON and decodes the response into out (if non-nil).
func (c *client) doJSON(ctx context.Context, method, path string, v, out any) error {
	var body []byte
	if v != nil {
		var err error
		if body, err = json.Marshal(v); err != nil {
			return err
		}
	}
	r, err := c.do(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	return decode(r, method, out)
}

func decode(r *response, method string, out any) error {
	if out != nil && len(r.Body) > 0 {
		if err := json.Unmarshal(r.Body, out); err != nil {
			return errs.Provider("invalidResponse", fmt.Errorf("decode %s: %w", method, err))
		}
	}
	return nil
}

func retryable(r *response) bool {
	switch r.Status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusForbidden:
		// Gmail signals per-user quota as 403 rateLimitExceeded.
		reason := parseAPIError(r.Body).reason()
		return reason == "rateLimitExceeded" || reason == "userRateLimitExceeded"
	}
	return false
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

type apiErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

func parseAPIError(b []byte) apiErrorBody {
	var e apiErrorBody
	_ = json.Unmarshal(b, &e)
	return e
}

func (e apiErrorBody) reason() string {
	if len(e.Error.Errors) > 0 {
		return e.Error.Errors[0].Reason
	}
	return ""
}

func (c *client) apiError(method, target string, r *response) error {
	ge := parseAPIError(r.Body)
	code := firstNonEmpty(ge.reason(), ge.Error.Status, strconv.Itoa(r.Status))
	msg := firstNonEmpty(ge.Error.Message, http.StatusText(r.Status))
	relogin := fmt.Sprintf("run `email auth login -a %s --provider gmail`", c.alias)
	if r.Status == http.StatusUnauthorized {
		e := errs.Auth("Gmail rejected the token (%s: %s); %s", code, msg, relogin)
		e.ProviderCode = code
		return e
	}
	if r.Status == http.StatusForbidden && (code == "insufficientPermissions" || ge.Error.Status == "PERMISSION_DENIED" && strings.Contains(msg, "scope")) {
		e := errs.Auth("the token lacks the scope for this call (%s); %s with a higher --mode", msg, relogin)
		e.ProviderCode = code
		return e
	}
	err := fmt.Errorf("Gmail %s %s: HTTP %d %s: %s", method, redactURL(target), r.Status, code, msg)
	if r.Status == http.StatusNotFound {
		return errs.NotFound(code, err)
	}
	return errs.Provider(code, err)
}

// isNotFound reports a 404 from the API.
func isNotFound(err error) bool {
	var e *errs.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == "not_found"
}

// redactURL drops query strings (they can carry page tokens and queries).
func redactURL(s string) string {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i] + "?…"
	}
	return s
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
