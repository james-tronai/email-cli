// Package provider defines the unified mail model and the capability
// interfaces that each backend (msgraph, gmail, imap, fake) implements.
//
// Interfaces are split by capability so that a backend only exposes what it
// actually supports; `email capabilities` is derived from type assertions.
package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Reserved label names. Every provider maps these onto its own folders/labels.
const (
	LabelInbox   = "inbox"
	LabelArchive = "archive"
	LabelSent    = "sent"
	LabelDrafts  = "drafts"
	LabelTrash   = "trash"
	LabelSpam    = "spam"
)

var ReservedLabels = []string{LabelInbox, LabelArchive, LabelSent, LabelDrafts, LabelTrash, LabelSpam}

type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

// Domain returns the lower-cased domain part of the address.
func (a Address) Domain() string {
	i := strings.LastIndexByte(a.Email, '@')
	if i < 0 {
		return ""
	}
	return strings.ToLower(a.Email[i+1:])
}

type AttachmentMeta struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime,omitempty"`
	Inline bool   `json:"inline"`
}

type Attachment struct {
	AttachmentMeta
	Data []byte `json:"-"`
}

// AttachmentFile is a local file to attach to a draft.
type AttachmentFile struct {
	Name string
	MIME string
	Data []byte
}

type Flags struct {
	Unread         bool `json:"unread"`
	Draft          bool `json:"draft"`
	Flagged        bool `json:"flagged"`
	HasAttachments bool `json:"has_attachments"`
}

// Message is the unified message model. IDs are the provider's native IDs,
// passed through untouched.
//
// Providers fill BodyText and/or BodyHTML with what the server returned; the
// service layer turns HTML into sanitized text (stripping hidden text) before
// anything reaches the caller.
type Message struct {
	ID          string            `json:"id"`
	ThreadID    string            `json:"thread_id,omitempty"`
	Provider    string            `json:"provider"`
	Account     string            `json:"account"`
	From        Address           `json:"from"`
	To          []Address         `json:"to,omitempty"`
	Cc          []Address         `json:"cc,omitempty"`
	Bcc         []Address         `json:"bcc,omitempty"`
	ReplyTo     []Address         `json:"reply_to,omitempty"`
	Date        time.Time         `json:"date"`
	Subject     string            `json:"subject"`
	Snippet     string            `json:"snippet,omitempty"`
	BodyText    string            `json:"body_text,omitempty"`
	BodyHTML    string            `json:"body_html,omitempty"`
	Attachments []AttachmentMeta  `json:"attachments,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	Flags       Flags             `json:"flags"`
	Headers     map[string]string `json:"headers,omitempty"`
	// WebLink opens the message in the provider's web client (for a draft,
	// in the compose/edit view). Empty when the provider has none.
	WebLink string `json:"web_link,omitempty"`

	// Set by the service layer.
	BodyHiddenTextRemoved bool `json:"body_hidden_text_removed,omitempty"`
	BodyTruncated         bool `json:"body_truncated,omitempty"`
	BodyLength            int  `json:"body_length,omitempty"` // full length in characters, set when truncated
}

type Page struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// MarshalJSON adds has_more so a caller that asked for N messages and got N
// can tell whether more exist without inspecting next_cursor.
func (p Page) MarshalJSON() ([]byte, error) {
	type plain Page
	return json.Marshal(struct {
		plain
		HasMore bool `json:"has_more"`
	}{plain(p), p.NextCursor != ""})
}

type ListOptions struct {
	Label  string
	Unread bool
	Since  time.Time
	Limit  int
	Cursor string
}

type SearchOptions struct {
	Query  string // provider-native: KQL for Graph $search, q= for Gmail
	Limit  int
	Cursor string
}

type GetOptions struct {
	IncludeHTML bool
	Headers     bool
}

type DraftInput struct {
	To, Cc, Bcc []Address
	Subject     string
	Body        string
	Attachments []AttachmentFile
}

// DraftUpdate changes only the non-nil fields.
type DraftUpdate struct {
	To, Cc, Bcc *[]Address
	Subject     *string
	Body        *string
	Attachments []AttachmentFile // appended
}

type ReplyInput struct {
	MessageID   string
	All         bool
	Body        string
	Attachments []AttachmentFile
}

type ForwardInput struct {
	MessageID string
	To        []Address
	Body      string
}

type MarkOptions struct {
	Read    *bool
	Flagged *bool
}

type SyncOptions struct {
	Label string
	State string // opaque cursor from the previous run; "" on first run
}

type SyncResult struct {
	Messages []Message
	State    string
	// Initialized is true when State was empty and the provider only
	// established a baseline without returning existing mail.
	Initialized bool
}

type SendResult struct {
	MessageID string    `json:"message_id,omitempty"`
	SentAt    time.Time `json:"sent_at"`
}

type RawRequest struct {
	Method string
	Path   string
	Body   []byte
}

type RawResponse struct {
	Status int
	Body   []byte
}

// Provider is the minimum every backend implements.
type Provider interface {
	Name() string
}

type Reader interface {
	List(ctx context.Context, opts ListOptions) (*Page, error)
	Get(ctx context.Context, id string, opts GetOptions) (*Message, error)
	Thread(ctx context.Context, threadID string, limit int) ([]Message, error)
}

type Searcher interface {
	Search(ctx context.Context, opts SearchOptions) (*Page, error)
}

type AttachmentReader interface {
	ListAttachments(ctx context.Context, messageID string) ([]AttachmentMeta, error)
	GetAttachment(ctx context.Context, messageID, attachmentID string) (*Attachment, error)
}

type Drafter interface {
	CreateDraft(ctx context.Context, in DraftInput) (*Message, error)
	CreateReplyDraft(ctx context.Context, in ReplyInput) (*Message, error)
	CreateForwardDraft(ctx context.Context, in ForwardInput) (*Message, error)
	UpdateDraft(ctx context.Context, id string, upd DraftUpdate) (*Message, error)
	ListDrafts(ctx context.Context, opts ListOptions) (*Page, error)
	DeleteDraft(ctx context.Context, id string) error
}

type Mover interface {
	Move(ctx context.Context, id, dest string) (*Message, error)
	Relabel(ctx context.Context, id string, add, remove []string) (*Message, error)
	Mark(ctx context.Context, id string, opts MarkOptions) (*Message, error)
}

type Syncer interface {
	Sync(ctx context.Context, opts SyncOptions) (*SyncResult, error)
}

type Sender interface {
	Send(ctx context.Context, draftID string) (*SendResult, error)
}

type RawAPI interface {
	Raw(ctx context.Context, req RawRequest) (*RawResponse, error)
}

// Capabilities lists the capability names a provider implements.
func Capabilities(p Provider) []string {
	var caps []string
	if _, ok := p.(Reader); ok {
		caps = append(caps, "read")
	}
	if _, ok := p.(Searcher); ok {
		caps = append(caps, "search")
	}
	if _, ok := p.(AttachmentReader); ok {
		caps = append(caps, "attachments")
	}
	if _, ok := p.(Drafter); ok {
		caps = append(caps, "draft")
	}
	if _, ok := p.(Mover); ok {
		caps = append(caps, "move")
	}
	if _, ok := p.(Syncer); ok {
		caps = append(caps, "sync")
	}
	if _, ok := p.(Sender); ok {
		caps = append(caps, "send")
	}
	if _, ok := p.(RawAPI); ok {
		caps = append(caps, "api")
	}
	return caps
}
