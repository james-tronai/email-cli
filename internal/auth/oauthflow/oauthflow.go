// Package oauthflow runs the browser half of an OAuth 2.0 authorization code
// + PKCE login for a public client. It is shared by msgraph and gmail.
//
// Two modes:
//
//   - loopback: listen on 127.0.0.1 (and ::1) at a random port, redirect to
//     http://localhost:<port>/callback, and catch the code when the browser
//     comes back. Needs a browser on the same machine.
//   - manual: for headless use (kubectl exec). The redirect goes to a port
//     nobody listens on; the browser shows a connection error and the user
//     pastes the address-bar URL (…/callback?code=…&state=…) on stdin.
//
// Either way the state is checked and the PKCE verifier is returned for the
// token exchange, which the provider does with its own library.
package oauthflow

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
)

const DefaultCallbackPath = "/callback"

// Prompt is printed (as JSON) so a person or agent can complete the login.
type Prompt struct {
	Mode         string `json:"mode"` // loopback | manual
	AuthorizeURL string `json:"authorize_url"`
	RedirectURI  string `json:"redirect_uri"`
	Instructions string `json:"instructions"`
}

type Params struct {
	// AuthorizeURL builds the provider's authorize URL.
	AuthorizeURL func(redirectURI, state, codeChallenge string) string
	Manual       bool
	CallbackPath string // default DefaultCallbackPath
	Stdin        io.Reader
	// OnPrompt is called once the URL is ready, before waiting.
	OnPrompt func(Prompt) error
	// OpenBrowser, when set, is called in loopback mode; failure is ignored
	// (the URL has been printed anyway).
	OpenBrowser func(url string) error
}

type Result struct {
	Code         string
	RedirectURI  string
	CodeVerifier string
}

// Run performs the browser step and returns the authorization code.
func Run(ctx context.Context, p Params) (*Result, error) {
	path := p.CallbackPath
	if path == "" {
		path = DefaultCallbackPath
	}
	state, err := randomString(24)
	if err != nil {
		return nil, err
	}
	verifier, err := randomString(48)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if p.Manual {
		return runManual(ctx, p, path, state, verifier, challenge)
	}
	return runLoopback(ctx, p, path, state, verifier, challenge)
}

func runManual(ctx context.Context, p Params, path, state, verifier, challenge string) (*Result, error) {
	port, err := randomPort()
	if err != nil {
		return nil, err
	}
	redirect := fmt.Sprintf("http://localhost:%d%s", port, path)
	prompt := Prompt{
		Mode:         "manual",
		AuthorizeURL: p.AuthorizeURL(redirect, state, challenge),
		RedirectURI:  redirect,
		Instructions: "Open authorize_url in any browser and sign in. The browser then fails to load " + redirect +
			"?code=… — copy the whole address-bar URL and paste it here, then press Enter.",
	}
	if p.OnPrompt != nil {
		if err := p.OnPrompt(prompt); err != nil {
			return nil, err
		}
	}
	if p.Stdin == nil {
		return nil, errs.Usage("--manual needs stdin (with kubectl exec, pass -i)")
	}
	line, err := readLine(ctx, p.Stdin)
	if err != nil {
		return nil, err
	}
	code, err := ParseRedirect(line, state)
	if err != nil {
		return nil, err
	}
	return &Result{Code: code, RedirectURI: redirect, CodeVerifier: verifier}, nil
}

// readLine reads one line from r, honouring ctx.
func readLine(ctx context.Context, r io.Reader) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := bufio.NewReaderSize(r, 16<<10).ReadString('\n')
		ch <- res{s, err}
	}()
	select {
	case <-ctx.Done():
		return "", errs.Auth("login timed out waiting for the redirect URL")
	case x := <-ch:
		if strings.TrimSpace(x.s) == "" {
			if x.err != nil && !errors.Is(x.err, io.EOF) {
				return "", x.err
			}
			return "", errs.Usage("no redirect URL on stdin (with kubectl exec, pass -i)")
		}
		return strings.TrimSpace(x.s), nil
	}
}

// ParseRedirect extracts the code from a pasted redirect URL (or just its
// query string) and checks the state.
func ParseRedirect(pasted, wantState string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	raw := pasted
	if i := strings.IndexByte(pasted, '?'); i >= 0 {
		raw = pasted[i+1:]
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", errs.Usage("cannot parse the pasted URL: %v", err)
	}
	return checkCallback(q, wantState)
}

func checkCallback(q url.Values, wantState string) (string, error) {
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		if len(desc) > 300 {
			desc = desc[:300] + "…"
		}
		return "", errs.Auth("authorization failed: %s: %s", e, desc)
	}
	if q.Get("state") != wantState {
		return "", errs.Auth("state mismatch: the URL is not from this login attempt; start the login again")
	}
	code := q.Get("code")
	if code == "" {
		return "", errs.Usage("the URL has no code parameter; paste the full address-bar URL after signing in")
	}
	return code, nil
}

func runLoopback(ctx context.Context, p Params, path, state, verifier, challenge string) (*Result, error) {
	l4, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("loopback listener: %w (use --manual)", err)
	}
	port := l4.Addr().(*net.TCPAddr).Port
	listeners := []net.Listener{l4}
	// "localhost" may resolve to ::1 in the browser; listen there too if we can.
	if l6, err := net.Listen("tcp", "[::1]:"+strconv.Itoa(port)); err == nil {
		listeners = append(listeners, l6)
	}
	redirect := fmt.Sprintf("http://localhost:%d%s", port, path)

	type cb struct {
		code string
		err  error
	}
	done := make(chan cb, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		code, err := checkCallback(r.URL.Query(), state)
		var e *errs.Error
		if errors.As(err, &e) && strings.HasPrefix(e.Message, "state mismatch") {
			http.Error(w, "state mismatch", http.StatusBadRequest) // not ours: keep waiting
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, page("Sign-in failed", "Return to the terminal for details."))
		} else {
			fmt.Fprint(w, page("Signed in", "You can close this window and return to the terminal."))
		}
		select {
		case done <- cb{code, err}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	for _, l := range listeners {
		go func(l net.Listener) { _ = srv.Serve(l) }(l)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	authURL := p.AuthorizeURL(redirect, state, challenge)
	if p.OnPrompt != nil {
		if err := p.OnPrompt(Prompt{
			Mode:         "loopback",
			AuthorizeURL: authURL,
			RedirectURI:  redirect,
			Instructions: "Sign in in the browser window that opened (or open authorize_url on this machine). Waiting for the redirect…",
		}); err != nil {
			return nil, err
		}
	}
	if p.OpenBrowser != nil {
		_ = p.OpenBrowser(authURL)
	}
	select {
	case <-ctx.Done():
		return nil, errs.Auth("login timed out waiting for the browser redirect")
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return &Result{Code: r.code, RedirectURI: redirect, CodeVerifier: verifier}, nil
	}
}

func page(title, body string) string {
	return "<!doctype html><meta charset=utf-8><title>email: " + title + "</title>" +
		"<body style=\"font-family:system-ui;margin:3em\"><h2>" + title + "</h2><p>" + body + "</p></body>"
}

// OpenBrowser opens url with the platform's default handler.
func OpenBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

const urlSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// randomString returns n characters from the PKCE unreserved set.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(urlSafe)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = urlSafe[v.Int64()]
	}
	return string(b), nil
}

func randomPort() (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(65535-49152))
	if err != nil {
		return 0, err
	}
	return 49152 + int(v.Int64()), nil
}
