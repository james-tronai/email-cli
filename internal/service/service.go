// Package service is the layer between the CLI and providers. Every
// operation goes through it: policy gate first, then the provider call, then
// the audit log. It also normalises bodies (HTML → sanitized text, truncation)
// and appends the signoff to agent-written text.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/james-tronai/email-cli/internal/audit"
	"github.com/james-tronai/email-cli/internal/auth"
	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/render"
)

const DefaultMaxBody = 20000

type Service struct {
	Account  *auth.Account
	Provider provider.Provider
	Policy   *policy.Policy
	Audit    *audit.Logger
	StateDir string
	DryRun   bool
}

// DryRunResult is returned instead of performing a write under --dry-run.
type DryRunResult struct {
	DryRun   bool            `json:"dry_run"`
	Op       policy.Op       `json:"op"`
	Account  string          `json:"account"`
	Decision policy.Decision `json:"decision"`
	Args     any             `json:"args"`
}

func (s *Service) EffectiveMode() policy.Mode {
	return s.Policy.EffectiveMode(s.Account.Mode)
}

// gate checks the policy and audits denials.
func (s *Service) gate(req policy.Request, target string) (policy.Decision, error) {
	req.AccountMode = s.Account.Mode
	req.AccountDomain = s.Account.Domain()
	d := s.Policy.Check(req)
	if !d.Allowed {
		s.log(audit.Entry{Op: string(req.Op), Target: target, Recipients: req.Recipients, Decision: "deny", Reason: d.Reason})
		return d, errs.Policy("%s", d.Reason)
	}
	return d, nil
}

// done audits the outcome of an allowed write.
func (s *Service) done(op policy.Op, target string, recipients []string, resultID string, err error) {
	if !policy.IsWrite(op) && op != policy.OpAttachmentGet {
		return
	}
	e := audit.Entry{Op: string(op), Target: target, Recipients: recipients, Decision: "allow", Result: "ok", ResultID: resultID}
	if err != nil {
		e.Result, e.Error = "error", err.Error()
	}
	s.log(e)
}

func (s *Service) log(e audit.Entry) {
	e.Account, e.Provider = s.Account.Alias, s.Account.Provider
	if err := s.Audit.Log(e); err != nil {
		fmt.Fprintf(os.Stderr, "warning: audit log: %v\n", err)
	}
}

func (s *Service) dryRun(op policy.Op, d policy.Decision, target string, recipients []string, args any) *DryRunResult {
	s.log(audit.Entry{Op: string(op), Target: target, Recipients: recipients, Decision: "allow", Result: "dry_run"})
	return &DryRunResult{DryRun: true, Op: op, Account: s.Account.Alias, Decision: d, Args: args}
}

func capability[T any](s *Service, name string) (T, error) {
	c, ok := s.Provider.(T)
	if !ok {
		var zero T
		return zero, errs.Unsupported("provider %s does not support %s", s.Provider.Name(), name)
	}
	return c, nil
}

func emails(as ...[]provider.Address) []string {
	var out []string
	for _, list := range as {
		for _, a := range list {
			out = append(out, a.Email)
		}
	}
	return out
}

// ---- read ----

type GetOptions struct {
	IncludeHTML bool
	Headers     bool
	MaxBody     int // 0 = DefaultMaxBody, <0 = unlimited
}

func (s *Service) List(ctx context.Context, o provider.ListOptions) (*provider.Page, error) {
	if _, err := s.gate(policy.Request{Op: policy.OpList}, ""); err != nil {
		return nil, err
	}
	r, err := capability[provider.Reader](s, "list")
	if err != nil {
		return nil, err
	}
	return r.List(ctx, o)
}

func (s *Service) Search(ctx context.Context, o provider.SearchOptions) (*provider.Page, error) {
	if _, err := s.gate(policy.Request{Op: policy.OpSearch}, ""); err != nil {
		return nil, err
	}
	r, err := capability[provider.Searcher](s, "search")
	if err != nil {
		return nil, err
	}
	return r.Search(ctx, o)
}

func (s *Service) Get(ctx context.Context, id string, o GetOptions) (*provider.Message, error) {
	return s.get(ctx, policy.OpGet, id, o)
}

func (s *Service) GetDraft(ctx context.Context, id string, o GetOptions) (*provider.Message, error) {
	return s.get(ctx, policy.OpDraftGet, id, o)
}

func (s *Service) get(ctx context.Context, op policy.Op, id string, o GetOptions) (*provider.Message, error) {
	if _, err := s.gate(policy.Request{Op: op}, id); err != nil {
		return nil, err
	}
	r, err := capability[provider.Reader](s, "get")
	if err != nil {
		return nil, err
	}
	m, err := r.Get(ctx, id, provider.GetOptions{IncludeHTML: o.IncludeHTML, Headers: o.Headers})
	if err != nil {
		return nil, err
	}
	NormalizeBody(m, o.IncludeHTML, o.MaxBody)
	return m, nil
}

func (s *Service) Thread(ctx context.Context, threadID string, limit int, o GetOptions) ([]provider.Message, error) {
	if _, err := s.gate(policy.Request{Op: policy.OpThread}, threadID); err != nil {
		return nil, err
	}
	r, err := capability[provider.Reader](s, "thread")
	if err != nil {
		return nil, err
	}
	msgs, err := r.Thread(ctx, threadID, limit)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		NormalizeBody(&msgs[i], o.IncludeHTML, o.MaxBody)
	}
	return msgs, nil
}

// NormalizeBody derives BodyText from HTML when HTML is present (so hidden
// text is stripped and the model reads what a human would see), drops raw
// HTML unless requested, and truncates.
func NormalizeBody(m *provider.Message, includeHTML bool, maxBody int) {
	if strings.TrimSpace(m.BodyHTML) != "" {
		text, removed := render.HTMLToText(m.BodyHTML)
		m.BodyText = text
		m.BodyHiddenTextRemoved = removed
	}
	if !includeHTML {
		m.BodyHTML = ""
	}
	if maxBody == 0 {
		maxBody = DefaultMaxBody
	}
	if maxBody > 0 {
		r := []rune(m.BodyText)
		if len(r) > maxBody {
			m.BodyText = string(r[:maxBody])
			m.BodyTruncated = true
			m.BodyLength = len(r)
		}
	}
}

// ---- attachments ----

func (s *Service) ListAttachments(ctx context.Context, id string) ([]provider.AttachmentMeta, error) {
	if _, err := s.gate(policy.Request{Op: policy.OpAttachmentList}, id); err != nil {
		return nil, err
	}
	r, err := capability[provider.AttachmentReader](s, "attachments")
	if err != nil {
		return nil, err
	}
	return r.ListAttachments(ctx, id)
}

type DownloadedFile struct {
	ID   string `json:"id"`
	Name string `json:"name"` // original (untrusted) name
	Path string `json:"path"` // sanitized local path
	Size int64  `json:"size"`
	MIME string `json:"mime,omitempty"`
}

type DownloadResult struct {
	MessageID string           `json:"message_id"`
	Files     []DownloadedFile `json:"files"`
}

func (r *DownloadResult) IDs() []string {
	out := make([]string, len(r.Files))
	for i, f := range r.Files {
		out[i] = f.Path
	}
	return out
}

// DownloadAttachments writes attachments (all, or just attachmentID) into
// outDir with sanitized names. Files are written 0600 and never executed.
func (s *Service) DownloadAttachments(ctx context.Context, id, attachmentID, outDir string) (*DownloadResult, error) {
	if outDir == "" {
		return nil, errs.Usage("--out is required")
	}
	if _, err := s.gate(policy.Request{Op: policy.OpAttachmentGet}, id); err != nil {
		return nil, err
	}
	r, err := capability[provider.AttachmentReader](s, "attachments")
	if err != nil {
		return nil, err
	}
	metas, err := r.ListAttachments(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}
	res := &DownloadResult{MessageID: id, Files: []DownloadedFile{}}
	for _, m := range metas {
		if attachmentID != "" && m.ID != attachmentID {
			continue
		}
		a, err := r.GetAttachment(ctx, id, m.ID)
		if err != nil {
			s.done(policy.OpAttachmentGet, id, nil, m.ID, err)
			return nil, err
		}
		path, err := writeUnique(outDir, SanitizeFilename(m.Name, m.ID), a.Data)
		s.done(policy.OpAttachmentGet, id, nil, m.ID, err)
		if err != nil {
			return nil, err
		}
		res.Files = append(res.Files, DownloadedFile{ID: m.ID, Name: m.Name, Path: path, Size: int64(len(a.Data)), MIME: m.MIME})
	}
	if attachmentID != "" && len(res.Files) == 0 {
		return nil, errs.Provider("notFound", fmt.Errorf("attachment %s not found on %s", attachmentID, id))
	}
	return res, nil
}

// SanitizeFilename keeps a safe basename: no path separators, no control or
// shell-hostile characters, no leading dots, max 200 bytes.
func SanitizeFilename(name, fallbackID string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == '/', r == '\\', r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|', r == '`', r == '$':
			return '_'
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".-")
	for len(name) > 200 {
		r := []rune(name)
		name = string(r[:len(r)-1])
	}
	if name == "" || name == "." {
		name = "attachment-" + SanitizeFilename(fallbackID, "x")
	}
	return name
}

func writeUnique(dir, name string, data []byte) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		cand := name
		if i > 0 {
			cand = stem + " (" + strconv.Itoa(i) + ")" + ext
		}
		path := filepath.Join(dir, cand)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return path, err
	}
	return "", fmt.Errorf("too many files named %s in %s", name, dir)
}

// ---- api escape hatch ----

type APIResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

func (s *Service) API(ctx context.Context, method, path string, body []byte) (any, error) {
	method = strings.ToUpper(method)
	if !strings.HasPrefix(path, "/") {
		return nil, errs.Usage("api path must start with /")
	}
	d, err := s.gate(policy.Request{Op: policy.OpAPI, APIMethod: method, APIPath: path}, method+" "+path)
	if err != nil {
		return nil, err
	}
	r, err := capability[provider.RawAPI](s, "api")
	if err != nil {
		return nil, err
	}
	write := method != "GET" && method != "HEAD"
	if s.DryRun && write {
		return s.dryRun(policy.OpAPI, d, method+" "+path, nil, map[string]any{"method": method, "path": path, "body": string(body)}), nil
	}
	resp, err := r.Raw(ctx, provider.RawRequest{Method: method, Path: path, Body: body})
	if write {
		s.done(policy.OpAPI, method+" "+path, nil, "", err)
	}
	if err != nil {
		return nil, err
	}
	out := &APIResult{Status: resp.Status}
	if json.Valid(resp.Body) {
		out.Body = resp.Body
	} else if len(resp.Body) > 0 {
		out.Body, _ = json.Marshal(string(resp.Body))
	}
	if resp.Status >= 400 {
		return out, errs.Provider(strconv.Itoa(resp.Status), fmt.Errorf("HTTP %d: %s", resp.Status, truncate(string(resp.Body), 500)))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
