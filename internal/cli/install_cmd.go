package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/james-tronai/email-cli/internal/errs"
)

const (
	repoSlug = "james-tronai/email-cli"
	// InstallCommand is what `version --check` suggests; it never runs it.
	installCommand = "curl -fsSL https://github.com/" + repoSlug + "/releases/latest/download/install.sh | sh"
)

func (a *app) versionCmd() *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use:   "version [--check]",
		Short: "Print version information (--check: also look up the latest release)",
		Long: `--check asks GitHub for the latest release and says whether it is newer.
It only reports: nothing is downloaded or replaced (rerun install.sh, brew
upgrade, or pull a new image to update).`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !check {
				return a.output(a.build)
			}
			return a.output(a.checkVersion(cmd.Context()))
		},
	}
	c.Flags().BoolVar(&check, "check", false, "look up the latest release (reports only, never updates)")
	return c
}

type versionCheck struct {
	BuildInfo
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Install         string `json:"install,omitempty"`
	CheckError      string `json:"check_error,omitempty"`
}

// checkVersion never fails the command: a version check that cannot reach
// GitHub says so in check_error.
func (a *app) checkVersion(ctx context.Context) versionCheck {
	out := versionCheck{BuildInfo: a.build}
	api := os.Getenv("EMAIL_RELEASES_API") // tests
	if api == "" {
		api = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/repos/"+repoSlug+"/releases/latest", nil)
	if err != nil {
		out.CheckError = err.Error()
		return out
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", a.userAgent())
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		out.CheckError = err.Error()
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out.CheckError = fmt.Sprintf("GitHub answered HTTP %d", resp.StatusCode)
		return out
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil || rel.TagName == "" {
		out.CheckError = "no tag_name in the latest release"
		return out
	}
	out.Latest = rel.TagName
	if newer(rel.TagName, a.build.Version) {
		out.UpdateAvailable = true
		out.Install = installCommand + " -s -- --version " + rel.TagName
	}
	return out
}

// newer reports whether release tag a is newer than b (vMAJOR.MINOR.PATCH;
// a pre-release suffix is ignored). A dev build is never out of date.
func newer(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// selfInstallCmd copies this binary somewhere else. The container image is
// FROM scratch, with no shell or cp, so an initContainer runs
// `email self-install --dest /opt/data/.local/bin` instead.
func (a *app) selfInstallCmd() *cobra.Command {
	var dest string
	c := &cobra.Command{
		Use:   "self-install --dest DIR",
		Short: "Copy this email binary to DIR/email (for initContainers using the image)",
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			if dest == "" {
				return errs.Usage("--dest is required")
			}
			res, err := selfInstall(dest)
			if err != nil {
				return err
			}
			res.Version = a.build.Version
			return a.output(res)
		},
	}
	c.Flags().StringVar(&dest, "dest", "", "directory to copy the binary into")
	return c
}

type selfInstallResult struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Unchanged bool   `json:"unchanged"`
}

func selfInstall(dest string) (*selfInstallResult, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if s, err := filepath.EvalSymlinks(self); err == nil {
		self = s
	}
	src, err := os.ReadFile(self)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(dest, "email")
	res := &selfInstallResult{Path: target}
	if cur, err := os.ReadFile(target); err == nil && sha256.Sum256(cur) == sha256.Sum256(src) {
		res.Unchanged = true
		return res, nil
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, errs.Usage("--dest: %v", err)
	}
	f, err := os.CreateTemp(dest, ".email-*")
	if err != nil {
		return nil, errs.Usage("--dest: %v", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := io.Copy(f, bytes.NewReader(src)); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, target); err != nil {
		return nil, err
	}
	return res, nil
}
