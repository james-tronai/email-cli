package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	emailcli "github.com/james-tronai/email-cli"
)

// CronJob is contrib/hermes/cron-email-triage.json.
type CronJob struct {
	Name       string   `json:"name"`
	Schedule   string   `json:"schedule"`
	Skills     []string `json:"skills"`
	Continuity bool     `json:"continuity"`
	Prompt     string   `json:"prompt"`
}

// DefaultCronJob is the released triage job.
func DefaultCronJob() (CronJob, error) {
	var j CronJob
	if err := json.Unmarshal(emailcli.HermesCronJob, &j); err != nil {
		return j, fmt.Errorf("cron-email-triage.json: %w", err)
	}
	return j, nil
}

// SeedOptions drive SeedCron.
type SeedOptions struct {
	Home      string // HERMES_HOME
	HermesBin string // the `hermes` CLI
	Deliver   string // platform:chat_id, origin, local, …
	Job       CronJob
	DryRun    bool
	// Run executes the hermes CLI; tests replace it.
	Run func(ctx context.Context, bin string, args, env []string) (string, error)
}

// SeedResult says what SeedCron did.
type SeedResult struct {
	Action  string   `json:"action"` // created | updated | unchanged
	JobID   string   `json:"job_id,omitempty"`
	Name    string   `json:"name"`
	Changed []string `json:"changed,omitempty"`
	Paused  bool     `json:"paused,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
	// Command is the hermes command line, with the prompt elided.
	Command []string `json:"command,omitempty"`
}

// hermesJob is the part of $HERMES_HOME/cron/jobs.json that seed-cron reads.
type hermesJob struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Prompt   string   `json:"prompt"`
	Skills   []string `json:"skills"`
	Skill    string   `json:"skill"`
	Deliver  string   `json:"deliver"`
	Context  []string `json:"context_from"`
	Enabled  *bool    `json:"enabled"`
	Schedule struct {
		Kind    string `json:"kind"`
		Minutes int    `json:"minutes"`
		Expr    string `json:"expr"`
		Display string `json:"display"`
	} `json:"schedule"`
	ScheduleDisplay string `json:"schedule_display"`
}

func readJobs(home string) ([]hermesJob, error) {
	b, err := os.ReadFile(filepath.Join(home, "cron", "jobs.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Jobs []hermesJob `json:"jobs"`
	}
	if err := json.Unmarshal(b, &wrapped); err == nil && wrapped.Jobs != nil {
		return wrapped.Jobs, nil
	}
	var list []hermesJob
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("cron/jobs.json: %w", err)
	}
	return list, nil
}

// findJob matches by name, case-insensitively, as Hermes resolves a cron_job
// reference.
func findJob(jobs []hermesJob, name string) (*hermesJob, error) {
	var found []hermesJob
	for _, j := range jobs {
		if strings.EqualFold(j.Name, name) {
			found = append(found, j)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &found[0], nil
	}
	ids := make([]string, len(found))
	for i, j := range found {
		ids[i] = j.ID
	}
	return nil, fmt.Errorf("%d cron jobs are named %q (%s); remove or rename the extra ones", len(found), name, strings.Join(ids, ", "))
}

var durationRE = regexp.MustCompile(`^(\d*)\s*(m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days)$`)

// intervalMinutes parses "every 2h" / "30m" the way Hermes does.
func intervalMinutes(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.TrimPrefix(s, "every "))
	m := durationRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n := 1
	if m[1] != "" {
		n, _ = strconv.Atoi(m[1])
	}
	switch m[2][0] {
	case 'h':
		n *= 60
	case 'd':
		n *= 1440
	}
	return n, true
}

func scheduleMatches(j *hermesJob, want string) bool {
	if n, ok := intervalMinutes(want); ok {
		return j.Schedule.Kind == "interval" && j.Schedule.Minutes == n
	}
	want = strings.TrimSpace(want)
	return j.Schedule.Expr == want || j.Schedule.Display == want || j.ScheduleDisplay == want
}

func (j *hermesJob) skills() []string {
	if len(j.Skills) > 0 {
		return j.Skills
	}
	if j.Skill != "" {
		return []string{j.Skill}
	}
	return nil
}

// SeedCron creates the job, or edits the job of that name so that it matches.
// A job that matches already is left alone (editing would move its next run),
// and a job the user paused stays paused.
func SeedCron(ctx context.Context, o SeedOptions) (*SeedResult, error) {
	if o.Home == "" {
		return nil, fmt.Errorf("HERMES_HOME is not set")
	}
	if o.Job.Name == "" || o.Job.Schedule == "" || strings.TrimSpace(o.Job.Prompt) == "" {
		return nil, fmt.Errorf("the cron job needs a name, a schedule and a prompt")
	}
	if o.Deliver == "" {
		return nil, fmt.Errorf("--deliver is required (for example discord:<chat_id>)")
	}
	if o.HermesBin == "" {
		o.HermesBin = "hermes"
	}
	if o.Run == nil {
		o.Run = runHermes
	}
	jobs, err := readJobs(o.Home)
	if err != nil {
		return nil, err
	}
	cur, err := findJob(jobs, o.Job.Name)
	if err != nil {
		return nil, err
	}
	res := &SeedResult{Name: o.Job.Name, DryRun: o.DryRun}
	var args []string
	if cur == nil {
		res.Action = "created"
		args = []string{"cron", "create", "--name", o.Job.Name, "--deliver", o.Deliver}
		for _, s := range o.Job.Skills {
			args = append(args, "--skill", s)
		}
		if o.Job.Continuity {
			args = append(args, "--continuity")
		}
		args = append(args, o.Job.Schedule, o.Job.Prompt)
	} else {
		res.JobID = cur.ID
		res.Paused = cur.Enabled != nil && !*cur.Enabled
		args = []string{"cron", "edit", cur.ID}
		if !scheduleMatches(cur, o.Job.Schedule) {
			res.Changed = append(res.Changed, "schedule")
			args = append(args, "--schedule", o.Job.Schedule)
		}
		if cur.Prompt != o.Job.Prompt {
			res.Changed = append(res.Changed, "prompt")
			args = append(args, "--prompt", o.Job.Prompt)
		}
		if cur.Deliver != o.Deliver {
			res.Changed = append(res.Changed, "deliver")
			args = append(args, "--deliver", o.Deliver)
		}
		if !slices.Equal(cur.skills(), o.Job.Skills) {
			res.Changed = append(res.Changed, "skills")
			if len(o.Job.Skills) == 0 {
				args = append(args, "--clear-skills")
			}
			for _, s := range o.Job.Skills {
				args = append(args, "--skill", s)
			}
		}
		if has := slices.Contains(cur.Context, "self"); has != o.Job.Continuity {
			res.Changed = append(res.Changed, "continuity")
			if o.Job.Continuity {
				args = append(args, "--continuity")
			} else {
				args = append(args, "--no-continuity")
			}
		}
		if len(res.Changed) == 0 {
			res.Action = "unchanged"
			return res, nil
		}
		res.Action = "updated"
	}
	res.Command = elidePrompt(append([]string{o.HermesBin}, args...), o.Job.Prompt)
	if o.DryRun {
		return res, nil
	}
	env := append(os.Environ(), "HERMES_HOME="+o.Home)
	if out, err := o.Run(ctx, o.HermesBin, args, env); err != nil {
		return nil, fmt.Errorf("hermes %s %s failed: %w\n%s", args[0], args[1], err, strings.TrimSpace(out))
	}
	if res.JobID == "" {
		if jobs, err := readJobs(o.Home); err == nil {
			if j, _ := findJob(jobs, o.Job.Name); j != nil {
				res.JobID = j.ID
			}
		}
	}
	return res, nil
}

func elidePrompt(cmd []string, prompt string) []string {
	out := slices.Clone(cmd)
	for i, a := range out {
		if a == prompt {
			out[i] = fmt.Sprintf("(prompt: %d characters)", len(prompt))
		}
	}
	return out
}

func runHermes(ctx context.Context, bin string, args, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
