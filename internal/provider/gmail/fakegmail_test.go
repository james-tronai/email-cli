package gmail

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

// fakeGmail is an httptest server that serves fixture files and records
// every request. Routes are matched on method + exact path, optionally on a
// query substring; the first match wins.
type fakeGmail struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes []route
	reqs   []recorded
}

type route struct {
	method, path, querySub string
	status                 int
	fixture                string // file under testdata
	body                   string // inline body when fixture == ""
	times                  int    // >0: only serve this many times
}

type recorded struct {
	Method, Path, RawQuery, Body string
	Header                       http.Header
}

func newFakeGmail(t *testing.T) *fakeGmail {
	f := &fakeGmail{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

const u = "/gmail/v1/users/me"

func (f *fakeGmail) on(r route) *fakeGmail {
	if r.status == 0 {
		r.status = 200
	}
	f.mu.Lock()
	f.routes = append(f.routes, r)
	f.mu.Unlock()
	return f
}

func (f *fakeGmail) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Body: string(body), Header: r.Header.Clone()})
	var hit *route
	for i := range f.routes {
		rt := &f.routes[i]
		if rt.method != r.Method || rt.path != r.URL.Path || rt.times < 0 {
			continue
		}
		if rt.querySub != "" && !strings.Contains(r.URL.RawQuery, rt.querySub) {
			continue
		}
		hit = rt
		if rt.times > 0 {
			if rt.times--; rt.times == 0 {
				rt.times = -1
			}
		}
		break
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if hit == nil {
		w.WriteHeader(404)
		b, _ := os.ReadFile("testdata/error_404.json")
		_, _ = w.Write(b)
		return
	}
	out := hit.body
	if hit.fixture != "" {
		b, err := os.ReadFile("testdata/" + hit.fixture)
		if err != nil {
			f.t.Errorf("fixture %s: %v", hit.fixture, err)
		}
		out = string(b)
	}
	w.WriteHeader(hit.status)
	_, _ = w.Write([]byte(out))
}

func (f *fakeGmail) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded{}, f.reqs...)
}

// find returns the recorded requests for method + path.
func (f *fakeGmail) find(method, path string) []recorded {
	var out []recorded
	for _, r := range f.requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGmail) one(method, path string) recorded {
	f.t.Helper()
	rs := f.find(method, path)
	if len(rs) != 1 {
		f.t.Fatalf("%s %s: %d requests, want 1", method, path, len(rs))
	}
	return rs[0]
}

// withLabels registers the label list most calls need.
func (f *fakeGmail) withLabels() *fakeGmail {
	return f.on(route{method: "GET", path: u + "/labels", fixture: "labels.json"})
}

func (f *fakeGmail) provider(t *testing.T) (*Provider, *[]time.Duration) {
	t.Helper()
	p, err := New(Config{Alias: "gw", Username: "james@tronai.example", BaseURL: f.srv.URL, Tokens: StaticToken("test-token"), UserAgent: "email-cli/test"})
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	p.c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	return p, &sleeps
}

func q(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
