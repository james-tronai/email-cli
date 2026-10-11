// Package hermes connects email to a Hermes agent: it writes the skill and
// the webhook route script into HERMES_HOME, seeds the triage cron job through
// the `hermes` CLI, and renders the watch service and the config.yaml snippet.
// Hermes itself is never modified beyond those files (docs/hermes.md).
package hermes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	emailcli "github.com/james-tronai/email-cli"
)

// Paths inside HERMES_HOME.
const (
	SkillPath          = "skills/comms/email/SKILL.md"
	RouteScriptPath    = "scripts/email-summary.py"
	RouteSettingsPath  = "scripts/email-summary.json"
	DefaultRouteName   = "email"
	DefaultWebhookPort = 8644
)

// FilesOptions customise what `email hermes files` writes.
type FilesOptions struct {
	// Notes are appended to the skill as "## This deployment": accounts,
	// house rules, language. The skill's own rules are not changed.
	Notes string
	// Timezone and Labels go to email-summary.json for the route script.
	Timezone string
	Labels   map[string]string
}

// FileResult reports one written file.
type FileResult struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Removed bool   `json:"removed,omitempty"`
}

// Skill returns the skill text, with the deployment notes appended.
func Skill(notes string) []byte {
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return emailcli.EmailSkill
	}
	var b bytes.Buffer
	b.Write(bytes.TrimRight(emailcli.EmailSkill, "\n"))
	b.WriteString("\n\n## This deployment\n\nWritten by the deployer. Where it differs from the rest of this skill, the rules at the top still win.\n\n")
	b.WriteString(notes)
	b.WriteString("\n")
	return b.Bytes()
}

// RouteSettings is email-summary.json, or nil when there is nothing to say.
func RouteSettings(o FilesOptions) ([]byte, error) {
	if o.Timezone != "" {
		if _, err := time.LoadLocation(o.Timezone); err != nil {
			return nil, fmt.Errorf("unknown time zone %q", o.Timezone)
		}
	}
	if o.Timezone == "" && len(o.Labels) == 0 {
		return nil, nil
	}
	s := struct {
		Timezone string            `json:"timezone,omitempty"`
		Labels   map[string]string `json:"labels,omitempty"`
	}{o.Timezone, o.Labels}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteFiles writes the skill, the route script and its settings into home.
// Files are replaced atomically and only when their content changes; the
// released files win over local edits. Without settings, an existing
// email-summary.json is removed so that a value taken out of the deployment
// does not linger.
func WriteFiles(home string, o FilesOptions) ([]FileResult, error) {
	if home == "" {
		return nil, fmt.Errorf("HERMES_HOME is not set")
	}
	settings, err := RouteSettings(o)
	if err != nil {
		return nil, err
	}
	var out []FileResult
	for _, f := range []struct {
		rel  string
		data []byte
	}{
		{SkillPath, Skill(o.Notes)},
		{RouteScriptPath, emailcli.HermesRouteScript},
		{RouteSettingsPath, settings},
	} {
		path := filepath.Join(home, filepath.FromSlash(f.rel))
		if f.data == nil {
			if err := os.Remove(path); err == nil {
				out = append(out, FileResult{Path: path, Changed: true, Removed: true})
			} else if !os.IsNotExist(err) {
				return out, err
			}
			continue
		}
		changed, err := WriteIfChanged(path, f.data, 0o644)
		if err != nil {
			return out, err
		}
		out = append(out, FileResult{Path: path, Changed: changed})
	}
	return out, nil
}

// WriteIfChanged writes data to path atomically unless it already holds
// exactly that, creating parent directories (0755).
func WriteIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		if st, err := os.Stat(path); err == nil && st.Mode().Perm() != mode {
			return true, os.Chmod(path, mode)
		}
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// ParseLabels turns ["work=Work mailbox", …] into a map.
func ParseLabels(kv []string) (map[string]string, error) {
	if len(kv) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, s := range kv {
		k, v, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("--account-label wants alias=label, got %q", s)
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, nil
}
