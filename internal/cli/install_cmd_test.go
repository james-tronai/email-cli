package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.7.0", "v0.6.0", true},
		{"v1.0.0", "v0.10.3", true},
		{"v0.6.0", "v0.6.0", false},
		{"v0.6.0", "v0.7.0", false},
		{"v0.7.0", "dev", false},
		{"v0.7.0-rc1", "v0.6.9", true},
		{"garbage", "v0.6.0", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestVersionCheck(t *testing.T) {
	newEnv(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/james-tronai/email-cli/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","name":"v9.9.9"}`))
	}))
	defer srv.Close()
	t.Setenv("EMAIL_RELEASES_API", srv.URL)
	code, stdout, stderr := runWithVersion(t, []string{"version", "--check"}, "v0.7.0")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var v versionCheck
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatal(err)
	}
	if v.Latest != "v9.9.9" || !v.UpdateAvailable || !strings.Contains(v.Install, "--version v9.9.9") || v.Version != "v0.7.0" {
		t.Fatalf("%+v", v)
	}

	// Unreachable: still exit 0, the reason in check_error.
	t.Setenv("EMAIL_RELEASES_API", "http://127.0.0.1:1")
	code, stdout, _ = runWithVersion(t, []string{"version", "--check"}, "v0.7.0")
	if code != 0 || !strings.Contains(stdout, "check_error") || strings.Contains(stdout, `"update_available": true`) {
		t.Fatalf("exit %d %s", code, stdout)
	}
}

// runWithVersion runs the CLI as a build of the given version.
func runWithVersion(t *testing.T, args []string, version string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	code := Execute(BuildInfo{Version: version}, args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestSelfInstall(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	res, err := selfInstall(dest)
	if err != nil || res.Unchanged {
		t.Fatalf("%+v %v", res, err)
	}
	fi, err := os.Stat(filepath.Join(dest, "email"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("%v %v", fi, err)
	}
	res, err = selfInstall(dest)
	if err != nil || !res.Unchanged {
		t.Fatalf("second run: %+v %v", res, err)
	}
}
