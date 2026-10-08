package msgraph

import (
	"net/textproto"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/provider"
)

// Fields fetched for list/search/sync (no body).
const listSelect = "id,conversationId,subject,bodyPreview,from,toRecipients,ccRecipients,replyTo," +
	"receivedDateTime,isRead,isDraft,flag,hasAttachments,categories,parentFolderId"

// Fields fetched for get/thread.
const getSelect = listSelect + ",bccRecipients,body,internetMessageId"

type gEmail struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type gMessage struct {
	ID               string    `json:"id"`
	ConversationID   string    `json:"conversationId"`
	Subject          string    `json:"subject"`
	BodyPreview      string    `json:"bodyPreview"`
	Body             *gBody    `json:"body"`
	From             *gEmail   `json:"from"`
	ToRecipients     []gEmail  `json:"toRecipients"`
	CcRecipients     []gEmail  `json:"ccRecipients"`
	BccRecipients    []gEmail  `json:"bccRecipients"`
	ReplyTo          []gEmail  `json:"replyTo"`
	ReceivedDateTime time.Time `json:"receivedDateTime"`
	IsRead           bool      `json:"isRead"`
	IsDraft          bool      `json:"isDraft"`
	Flag             struct {
		FlagStatus string `json:"flagStatus"`
	} `json:"flag"`
	HasAttachments         bool     `json:"hasAttachments"`
	Categories             []string `json:"categories"`
	ParentFolderID         string   `json:"parentFolderId"`
	InternetMessageID      string   `json:"internetMessageId"`
	InternetMessageHeaders []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"internetMessageHeaders"`
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

type gBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type gPage struct {
	Value     []gMessage `json:"value"`
	NextLink  string     `json:"@odata.nextLink"`
	DeltaLink string     `json:"@odata.deltaLink"`
}

func addr(e *gEmail) provider.Address {
	if e == nil {
		return provider.Address{}
	}
	return provider.Address{Name: e.EmailAddress.Name, Email: e.EmailAddress.Address}
}

func addrs(es []gEmail) []provider.Address {
	if len(es) == 0 {
		return nil
	}
	out := make([]provider.Address, len(es))
	for i := range es {
		out[i] = addr(&es[i])
	}
	return out
}

func toRecipients(as []provider.Address) []gEmail {
	out := make([]gEmail, len(as))
	for i, a := range as {
		out[i].EmailAddress.Name = a.Name
		out[i].EmailAddress.Address = a.Email
	}
	return out
}

// headerAllow is what --headers returns.
var headerAllow = map[string]bool{
	"message-id": true, "in-reply-to": true, "references": true, "authentication-results": true,
	"return-path": true, "list-unsubscribe": true, "reply-to": true,
}

// convert maps a Graph message. folderLabel is the unified label of the
// parent folder ("" if unknown).
func (p *Provider) convert(g *gMessage, folderLabel string) provider.Message {
	m := provider.Message{
		ID:       g.ID,
		ThreadID: g.ConversationID,
		Provider: Name,
		Account:  p.alias,
		From:     addr(g.From),
		To:       addrs(g.ToRecipients),
		Cc:       addrs(g.CcRecipients),
		Bcc:      addrs(g.BccRecipients),
		ReplyTo:  addrs(g.ReplyTo),
		Date:     g.ReceivedDateTime,
		Subject:  g.Subject,
		Snippet:  snippet(g.BodyPreview),
		Flags: provider.Flags{
			Unread:         !g.IsRead,
			Draft:          g.IsDraft,
			Flagged:        g.Flag.FlagStatus == "flagged",
			HasAttachments: g.HasAttachments,
		},
	}
	if folderLabel != "" {
		m.Labels = append(m.Labels, folderLabel)
	}
	m.Labels = append(m.Labels, g.Categories...)
	if g.Body != nil {
		if strings.EqualFold(g.Body.ContentType, "html") {
			m.BodyHTML = g.Body.Content
		} else {
			m.BodyText = g.Body.Content
		}
	}
	if len(g.InternetMessageHeaders) > 0 || g.InternetMessageID != "" {
		// Keys are canonical MIME form (Message-Id, In-Reply-To) so the
		// internetMessageId property and the raw header don't both appear.
		h := map[string]string{}
		for _, x := range g.InternetMessageHeaders {
			k := textproto.CanonicalMIMEHeaderKey(x.Name)
			if _, dup := h[k]; !dup && headerAllow[strings.ToLower(x.Name)] {
				h[k] = x.Value
			}
		}
		if g.InternetMessageID != "" {
			h["Message-Id"] = g.InternetMessageID
		}
		m.Headers = h
	}
	return m
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200])
	}
	return s
}
