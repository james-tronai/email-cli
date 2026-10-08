package msgraph

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGraph is an httptest server that serves fixture files and records
// every request. Routes are "METHOD /path" (path without the /v1.0 prefix);
// a route may also match on a query substring via "METHOD /path?substr".
type fakeGraph struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes []route
	reqs   []recorded
}

type route struct {
	method, path, querySub string
	status                 int
	fixture                string            // file under testdata, {{BASE}} replaced
	body                   string            // inline body when fixture == ""
	header                 map[string]string // response headers
	times                  int               // >0: only serve this many times
}

type recorded struct {
	Method, Path, RawQuery, Body string
	Header                       http.Header
}

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) base() string { return f.srv.URL + "/v1.0" }

func (f *fakeGraph) on(r route) *fakeGraph {
	if r.status == 0 {
		r.status = 200
	}
	f.mu.Lock()
	f.routes = append(f.routes, r)
	f.mu.Unlock()
	return f
}

func (f *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimPrefix(r.URL.Path, "/v1.0")
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: path, RawQuery: r.URL.RawQuery, Body: string(body), Header: r.Header.Clone()})
	var hit *route
	for i := range f.routes {
		rt := &f.routes[i]
		if rt.method != r.Method || rt.path != path || rt.times < 0 {
			continue
		}
		if rt.querySub != "" && !strings.Contains(r.URL.RawQuery, rt.querySub) {
			continue
		}
		hit = rt
		if rt.times > 0 {
			rt.times--
			if rt.times == 0 {
				rt.times = -1
			}
		}
		break
	}
	f.mu.Unlock()
	if hit == nil {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"ErrorItemNotFound","message":"no fake route for ` + r.Method + " " + path + `"}}`))
		return
	}
	out := hit.body
	if hit.fixture != "" {
		b, err := os.ReadFile("testdata/" + hit.fixture)
		if err != nil {
			f.t.Errorf("fixture %s: %v", hit.fixture, err)
		}
		out = strings.ReplaceAll(string(b), "{{BASE}}", f.base())
	}
	for k, v := range hit.header {
		w.Header().Set(k, v)
	}
	w.Header().Set("request-id", "req-"+hit.method+"-test")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(hit.status)
	_, _ = w.Write([]byte(out))
}

func (f *fakeGraph) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded{}, f.reqs...)
}

func (f *fakeGraph) last() recorded {
	rs := f.requests()
	if len(rs) == 0 {
		f.t.Fatal("no requests")
	}
	return rs[len(rs)-1]
}

// provider wired to the fake server, with sleeps recorded instead of taken.
func (f *fakeGraph) provider(t *testing.T) (*Provider, *[]time.Duration) {
	t.Helper()
	p, err := New(Config{Alias: "work", Username: "james@tronai.example", BaseURL: f.base(), Tokens: StaticToken("test-token"), UserAgent: "email-cli/test"})
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	p.c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	// Pre-seed folder ids so tests don't need the six well-known lookups.
	p.folders = &folderCache{ReservedLoaded: true, Labels: map[string]string{
		"AAMkAD-folder-inbox": "inbox", "AAMkAD-folder-drafts": "drafts",
	}}
	return p, &sleeps
}
