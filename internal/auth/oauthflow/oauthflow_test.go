package oauthflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
)

type captured struct {
	redirect, state, challenge string
}

func builder(c *captured) func(string, string, string) string {
	return func(redirect, state, challenge string) string {
		c.redirect, c.state, c.challenge = redirect, state, challenge
		return "https://idp.example/authorize?state=" + state
	}
}

func checkPKCE(t *testing.T, c *captured, r *Result) {
	t.Helper()
	sum := sha256.Sum256([]byte(r.CodeVerifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
		t.Fatal("challenge is not S256(verifier)")
	}
	if len(r.CodeVerifier) < 43 || len(r.CodeVerifier) > 128 {
		t.Fatalf("verifier length %d", len(r.CodeVerifier))
	}
	if r.RedirectURI != c.redirect || !strings.HasPrefix(r.RedirectURI, "http://localhost:") || !strings.HasSuffix(r.RedirectURI, "/callback") {
		t.Fatalf("redirect %q", r.RedirectURI)
	}
}

// manual runs the manual flow, building the pasted line from the captured state.
func manual(t *testing.T, paste func(c *captured) string) (*Result, *captured, error) {
	t.Helper()
	c := &captured{}
	pr, pw := io.Pipe()
	var prompt Prompt
	ready := make(chan struct{})
	go func() {
		<-ready // the prompt (and so the state) exists; paste
		_, _ = pw.Write([]byte(paste(c) + "\n"))
		pw.Close()
	}()
	r, err := Run(context.Background(), Params{
		AuthorizeURL: builder(c),
		Manual:       true,
		Stdin:        pr,
		OnPrompt:     func(p Prompt) error { prompt = p; close(ready); return nil },
	})
	if prompt.Mode != "manual" || !strings.HasPrefix(prompt.AuthorizeURL, "https://idp.example/") {
		t.Fatalf("prompt: %+v", prompt)
	}
	return r, c, err
}

func TestManualHappyPath(t *testing.T) {
	r, c, err := manual(t, func(c *captured) string {
		return c.redirect + "?code=abc123&state=" + url.QueryEscape(c.state) + "&session_state=x"
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Code != "abc123" {
		t.Fatalf("code %q", r.Code)
	}
	checkPKCE(t, c, r)
}

func TestManualStateMismatch(t *testing.T) {
	_, _, err := manual(t, func(c *captured) string { return c.redirect + "?code=abc&state=forged" })
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("%v", err)
	}
}

func TestManualProviderError(t *testing.T) {
	_, _, err := manual(t, func(c *captured) string {
		return c.redirect + "?error=access_denied&error_description=AADSTS65004%3A+User+declined&state=" + c.state
	})
	if errs.ExitCode(err) != errs.ExitAuth || !strings.Contains(err.Error(), "AADSTS65004") {
		t.Fatalf("%v", err)
	}
}

func TestManualEmptyStdin(t *testing.T) {
	_, err := Run(context.Background(), Params{AuthorizeURL: builder(&captured{}), Manual: true, Stdin: strings.NewReader("")})
	if errs.ExitCode(err) != errs.ExitUsage {
		t.Fatalf("%v", err)
	}
}

func TestParseRedirectAcceptsQueryOnly(t *testing.T) {
	code, err := ParseRedirect("  code=xyz&state=s1  ", "s1")
	if err != nil || code != "xyz" {
		t.Fatalf("%q %v", code, err)
	}
}

func TestLoopback(t *testing.T) {
	c := &captured{}
	var opened string
	type out struct {
		r   *Result
		err error
	}
	ch := make(chan out, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan string, 1)
	go func() {
		r, err := Run(ctx, Params{
			AuthorizeURL: builder(c),
			OnPrompt:     func(p Prompt) error { ready <- p.RedirectURI; return nil },
			OpenBrowser:  func(u string) error { opened = u; return nil },
		})
		ch <- out{r, err}
	}()
	base := strings.Replace(<-ready, "localhost", "127.0.0.1", 1)

	// A request with the wrong state is rejected and the flow keeps waiting.
	resp, err := http.Get(base + "?code=evil&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("forged state status %d", resp.StatusCode)
	}
	resp, err = http.Get(base + "?code=good&state=" + url.QueryEscape(c.state))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Signed in") {
		t.Fatalf("page: %s", body)
	}
	o := <-ch // happens-after opened/c writes in Run
	if o.err != nil || o.r.Code != "good" {
		t.Fatalf("%+v %v", o.r, o.err)
	}
	if !strings.HasPrefix(opened, "https://idp.example/") {
		t.Fatalf("browser not opened: %q", opened)
	}
	checkPKCE(t, c, o.r)
}

func TestLoopbackTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, Params{AuthorizeURL: builder(&captured{})})
	if errs.ExitCode(err) != errs.ExitAuth {
		t.Fatalf("%v", err)
	}
}
