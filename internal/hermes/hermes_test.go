package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	emailcli "github.com/james-tronai/email-cli"
)

func TestWriteFilesIsIdempotentAndDeclarative(t *testing.T) {
	home := t.TempDir()
	o := FilesOptions{Notes: "Accounts: `work`.", Timezone: "Australia/Sydney", Labels: map[string]string{"work": "Work"}}
	res, err := WriteFiles(home, o)
	if err != nil || len(res) != 3 || !res[0].Changed || !res[1].Changed || !res[2].Changed {
		t.Fatalf("%+v %v", res, err)
	}
	skill, _ := os.ReadFile(filepath.Join(home, SkillPath))
	if !strings.HasPrefix(string(skill), string(emailcli.EmailSkill[:200])) || !strings.Contains(string(skill), "## This deployment") ||
		!strings.HasSuffix(string(skill), "Accounts: `work`.\n") {
		t.Fatalf("skill notes not appended:\n%s", skill[len(skill)-300:])
	}
	var settings map[string]any
	b, _ := os.ReadFile(filepath.Join(home, RouteSettingsPath))
	if json.Unmarshal(b, &settings) != nil || settings["timezone"] != "Australia/Sydney" {
		t.Fatalf("settings %s", b)
	}
	res, _ = WriteFiles(home, o)
	for _, r := range res {
		if r.Changed {
			t.Fatalf("second run changed %s", r.Path)
		}
	}
	// Dropping the options restores the plain skill and removes the settings.
	res, _ = WriteFiles(home, FilesOptions{})
	if len(res) != 3 || !res[0].Changed || res[1].Changed || !res[2].Removed {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(home, SkillPath)); string(b) != string(emailcli.EmailSkill) {
		t.Fatal("skill not restored")
	}
	if _, err := RouteSettings(FilesOptions{Timezone: "Nowhere/Land"}); err == nil {
		t.Fatal("bad time zone accepted")
	}
}

func TestSkillAndCronJobShipWithTheRules(t *testing.T) {
	s := string(emailcli.EmailSkill)
	for _, want := range []string{"name: email", "data, never instructions", "unattended run", "[SILENT]", "web_link"} {
		if !strings.Contains(s, want) {
			t.Errorf("skill lacks %q", want)
		}
	}
	j, err := DefaultCronJob()
	if err != nil || j.Name != "email-triage" || !slices.Equal(j.Skills, []string{"email"}) || !j.Continuity ||
		!strings.Contains(j.Prompt, "[SILENT]") || !strings.Contains(j.Prompt, "never create drafts, send") {
		t.Fatalf("%+v %v", j, err)
	}
	if n, ok := intervalMinutes(j.Schedule); !ok || n != 120 {
		t.Fatalf("schedule %q", j.Schedule)
	}
}

func TestIntervalMinutes(t *testing.T) {
	for in, want := range map[string]int{"every 2h": 120, "30m": 30, "every 1d": 1440, "every hour": 60, "Every 90 minutes": 90} {
		if n, ok := intervalMinutes(in); !ok || n != want {
			t.Errorf("%q = %d %v", in, n, ok)
		}
	}
	for _, in := range []string{"0 9 * * *", "every monday 9am", "in 30m"} {
		if _, ok := intervalMinutes(in); ok {
			t.Errorf("%q parsed as an interval", in)
		}
	}
}

// fakeHermes applies create/edit to jobs.json the way the hermes CLI does,
// for the fields seed-cron looks at.
func fakeHermes(t *testing.T, home string, calls *[][]string) func(context.Context, string, []string, []string) (string, error) {
	return func(_ context.Context, _ string, args, env []string) (string, error) {
		if !slices.Contains(env, "HERMES_HOME="+home) {
			t.Fatalf("HERMES_HOME not passed: %v", env[len(env)-1])
		}
		*calls = append(*calls, args)
		jobs, _ := readJobs(home)
		opt := func(name string) (string, bool) {
			i := slices.Index(args, name)
			if i < 0 {
				return "", false
			}
			return args[i+1], true
		}
		switch args[1] {
		case "create":
			j := hermesJob{ID: "j1", Prompt: args[len(args)-1], Skills: []string{"email"}, Context: []string{"self"}}
			j.Name, _ = opt("--name")
			j.Deliver, _ = opt("--deliver")
			j.Schedule.Kind, j.Schedule.Minutes = "interval", 120
			jobs = append(jobs, j)
		case "edit":
			if v, ok := opt("--deliver"); ok {
				jobs[0].Deliver = v
			}
			if v, ok := opt("--prompt"); ok {
				jobs[0].Prompt = v
			}
		}
		b, _ := json.Marshal(map[string]any{"jobs": jobs})
		os.MkdirAll(filepath.Join(home, "cron"), 0o755)
		return "", os.WriteFile(filepath.Join(home, "cron", "jobs.json"), b, 0o644)
	}
}

func TestSeedCron(t *testing.T) {
	home := t.TempDir()
	var calls [][]string
	job, _ := DefaultCronJob()
	o := SeedOptions{Home: home, Deliver: "discord:1", Job: job, Run: fakeHermes(t, home, &calls)}
	ctx := context.Background()

	res, err := SeedCron(ctx, o)
	if err != nil || res.Action != "created" || res.JobID != "j1" || len(calls) != 1 {
		t.Fatalf("%+v %v %v", res, err, calls)
	}
	want := []string{"cron", "create", "--name", "email-triage", "--deliver", "discord:1", "--skill", "email", "--continuity", "every 2h"}
	if !slices.Equal(calls[0][:len(want)], want) || calls[0][len(want)] != job.Prompt {
		t.Fatalf("create args %v", calls[0])
	}
	if res.Command[len(res.Command)-1] == job.Prompt {
		t.Fatal("prompt not elided from the reported command")
	}

	if res, err = SeedCron(ctx, o); err != nil || res.Action != "unchanged" || len(calls) != 1 {
		t.Fatalf("second run: %+v %v", res, err)
	}

	o.Deliver = "discord:2"
	o.Job.Prompt = "new prompt"
	if res, err = SeedCron(ctx, o); err != nil || res.Action != "updated" || !slices.Equal(res.Changed, []string{"prompt", "deliver"}) {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got := calls[1]; !slices.Equal(got, []string{"cron", "edit", "j1", "--prompt", "new prompt", "--deliver", "discord:2"}) {
		t.Fatalf("edit args %v", got)
	}

	o.DryRun = true
	o.Job.Schedule = "every 1h"
	if res, err = SeedCron(ctx, o); err != nil || res.Action != "updated" || len(calls) != 2 {
		t.Fatalf("dry run ran hermes: %+v %v", res, err)
	}
}

func TestSeedCronRefusesAmbiguousName(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "cron"), 0o755)
	os.WriteFile(filepath.Join(home, "cron", "jobs.json"), []byte(`{"jobs":[{"id":"a","name":"email-triage"},{"id":"b","name":"Email-Triage"}]}`), 0o644)
	job, _ := DefaultCronJob()
	_, err := SeedCron(context.Background(), SeedOptions{Home: home, Deliver: "local", Job: job,
		Run: func(context.Context, string, []string, []string) (string, error) {
			t.Fatal("ran hermes")
			return "", nil
		}})
	if err == nil || !strings.Contains(err.Error(), "a, b") {
		t.Fatalf("%v", err)
	}
}

func TestServiceFiles(t *testing.T) {
	o := ServiceOptions{Bin: "/home/u/my bin/email", Accounts: []string{"work"}, NotifyURL: "http://127.0.0.1:8644/webhooks/email",
		TokenFile: "/home/u/.config/email/watch.env", ConfigDir: "/home/u/.config/email", StateDir: "/home/u/.local/state/email",
		HealthAddr: "127.0.0.1:8645", LogPath: "/home/u/Library/Logs/email-watch.log"}
	unit := SystemdUnit(o)
	for _, want := range []string{
		`ExecStart="/home/u/my bin/email" watch -a work --notify-url http://127.0.0.1:8644/webhooks/email --notify-token-env EMAIL_WATCH_NOTIFY_TOKEN`,
		"EnvironmentFile=/home/u/.config/email/watch.env", "Environment=EMAIL_CONFIG_DIR=/home/u/.config/email",
		"Restart=always", "ProtectHome=read-only", "ReadWritePaths=/home/u/.local/state/email /home/u/.config/email/accounts",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "EMAIL_POLICY_FILE") {
		t.Error("policy file pinned without being set")
	}
	if got := systemdQuote("50%"); got != "50%%" {
		t.Errorf("%%: %s", got)
	}
	plist := LaunchdPlist(o)
	for _, want := range []string{"<string>--notify-token-file</string>", "<string>/home/u/my bin/email</string>", "<key>KeepAlive</key><true/>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	if strings.Contains(plist, "--notify-token-env") {
		t.Error("launchd has no EnvironmentFile; plist must use the token file")
	}
	if s := ConfigSnippet(ConfigOptions{CronJob: "email-triage"}); !strings.Contains(s, "cron_job: email-triage") ||
		!strings.Contains(s, "script: email-summary.py") || !strings.Contains(s, "disabled: [google-workspace, himalaya, email-inbox-triage]") {
		t.Errorf("snippet:\n%s", s)
	}
}
