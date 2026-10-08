package policy

import (
	"fmt"
	"strings"
)

// Op is an operation the CLI can perform.
type Op string

const (
	OpList           Op = "list"
	OpSearch         Op = "search"
	OpGet            Op = "get"
	OpThread         Op = "thread"
	OpAttachmentList Op = "attachments.list"
	OpAttachmentGet  Op = "attachments.download"
	OpSync           Op = "sync"
	OpDraftList      Op = "draft.list"
	OpDraftGet       Op = "draft.get"
	OpDraftCreate    Op = "draft.create"
	OpDraftReply     Op = "draft.reply"
	OpDraftForward   Op = "draft.forward"
	OpDraftUpdate    Op = "draft.update"
	OpDraftDelete    Op = "draft.delete"
	OpMove           Op = "move"
	OpLabel          Op = "label"
	OpMark           Op = "mark"
	OpSend           Op = "send"
	OpAPI            Op = "api"
)

var requiredMode = map[Op]Mode{
	OpList:           ModeRead,
	OpSearch:         ModeRead,
	OpGet:            ModeRead,
	OpThread:         ModeRead,
	OpAttachmentList: ModeRead,
	OpAttachmentGet:  ModeRead,
	OpSync:           ModeRead,
	OpDraftList:      ModeRead,
	OpDraftGet:       ModeRead,
	OpDraftCreate:    ModeDraft,
	OpDraftReply:     ModeDraft,
	OpDraftForward:   ModeDraft,
	OpDraftUpdate:    ModeDraft,
	OpDraftDelete:    ModeDraft,
	OpMove:           ModeDraft,
	OpLabel:          ModeDraft,
	OpMark:           ModeDraft,
	OpSend:           ModeSend,
	// OpAPI depends on the HTTP method; see apiMode.
}

// IsWrite reports whether op changes mailbox state (and is therefore audited).
func IsWrite(op Op) bool {
	return requiredMode[op] >= ModeDraft || op == OpAPI
}

// Request describes one operation to be checked.
type Request struct {
	Op            Op
	AccountMode   Mode     // mode the account logged in with (bounds the token's scopes)
	AccountDomain string   // domain of the logged-in mailbox, used as "internal" when no allowlist is set
	Recipients    []string // email addresses the operation would address
	Dest          string   // move destination
	AddLabels     []string // label additions
	HasReplyCtx   bool     // send: the draft is a reply/forward of an existing message
	APIMethod     string
	APIPath       string
}

type Decision struct {
	Allowed       bool   `json:"allowed"`
	Op            Op     `json:"op"`
	EffectiveMode Mode   `json:"effective_mode"`
	Reason        string `json:"reason,omitempty"`
}

func (p *Policy) EffectiveMode(accountMode Mode) Mode {
	return min(p.Default.Mode, accountMode)
}

// Check decides whether req is allowed.
func (p *Policy) Check(req Request) Decision {
	eff := p.EffectiveMode(req.AccountMode)
	d := Decision{Op: req.Op, EffectiveMode: eff}
	deny := func(format string, args ...any) Decision {
		d.Reason = fmt.Sprintf(format, args...)
		return d
	}

	need, ok := requiredMode[req.Op]
	if req.Op == OpAPI {
		need, ok = p.apiMode(req.APIMethod, req.APIPath), true
		if need == ModeNone {
			return deny("api %s is not allowed (delete disabled by policy)", strings.ToUpper(req.APIMethod))
		}
	}
	if !ok {
		return deny("unknown operation %q", req.Op)
	}
	if eff < need {
		return deny("operation %s disabled by policy: requires mode %s, effective mode is %s (policy %s, account login %s)",
			req.Op, need, eff, p.Default.Mode, req.AccountMode)
	}

	if !p.Delete.Enabled {
		if req.Op == OpMove && strings.EqualFold(req.Dest, "trash") {
			return deny("moving to trash is disabled by policy ([delete] enabled = false)")
		}
		for _, l := range req.AddLabels {
			if strings.EqualFold(l, "trash") {
				return deny("labelling as trash is disabled by policy ([delete] enabled = false)")
			}
		}
	}

	switch req.Op {
	case OpDraftForward:
		if p.Send.BlockExternalForward {
			if ext := p.external(req.Recipients, req.AccountDomain); len(ext) > 0 {
				return deny("forwarding to external recipients is blocked by policy: %s", strings.Join(ext, ", "))
			}
		}
	case OpSend:
		if p.Send.RequireReplyContext && !req.HasReplyCtx {
			return deny("policy requires sends to be replies or forwards of an existing message")
		}
		if p.Send.MaxRecipients > 0 && len(req.Recipients) > p.Send.MaxRecipients {
			return deny("%d recipients exceeds policy max_recipients = %d", len(req.Recipients), p.Send.MaxRecipients)
		}
		if len(p.Send.AllowedRecipientDomains) > 0 {
			if ext := p.external(req.Recipients, req.AccountDomain); len(ext) > 0 {
				return deny("recipients outside allowed_recipient_domains: %s", strings.Join(ext, ", "))
			}
		}
	}

	d.Allowed = true
	return d
}

// apiMode is the mode required for a raw API call. GET is read; DELETE needs
// [delete] enabled; any path that looks like a send endpoint needs send mode
// (this matters for Gmail, whose draft-mode scope gmail.modify can send).
func (p *Policy) apiMode(method, path string) Mode {
	if isSendPath(path) {
		return ModeSend
	}
	switch strings.ToUpper(method) {
	case "GET", "HEAD":
		return ModeRead
	case "DELETE":
		if !p.Delete.Enabled {
			return ModeNone
		}
		return ModeDraft
	default:
		return ModeDraft
	}
}

func isSendPath(path string) bool {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	for _, seg := range strings.Split(strings.ToLower(path), "/") {
		// Graph: /me/sendMail, /messages/{id}/send, /reply, /replyAll, /forward
		// (the latter three send immediately; createReply etc. do not).
		// Gmail: /messages/send, /drafts/send.
		switch seg {
		case "send", "sendmail", "reply", "replyall", "forward":
			return true
		}
	}
	return false
}

// external returns recipients outside the allowed domains. With no allowlist,
// the account's own domain is the only internal one.
func (p *Policy) external(recipients []string, accountDomain string) []string {
	allowed := p.Send.AllowedRecipientDomains
	if len(allowed) == 0 && accountDomain != "" {
		allowed = []string{strings.ToLower(accountDomain)}
	}
	var out []string
	for _, r := range recipients {
		i := strings.LastIndexByte(r, '@')
		dom := ""
		if i >= 0 {
			dom = strings.ToLower(r[i+1:])
		}
		if !domainAllowed(dom, allowed) {
			out = append(out, r)
		}
	}
	return out
}

func domainAllowed(dom string, allowed []string) bool {
	if dom == "" {
		return false
	}
	for _, a := range allowed {
		if dom == a {
			return true
		}
	}
	return false
}
