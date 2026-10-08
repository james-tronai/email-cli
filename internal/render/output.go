package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/itchyny/gojq"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatText Format = "text"
	FormatID   Format = "id"
)

func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatJSON, FormatText, FormatID:
		return Format(s), nil
	}
	return "", errs.Usage("invalid --format %q (want json, text or id)", s)
}

type Options struct {
	Format Format
	JQ     string
}

// Write renders v. --jq applies to the JSON form regardless of --format;
// string results are printed raw (like gh --jq).
func Write(w io.Writer, v any, o Options) error {
	if o.JQ != "" {
		return writeJQ(w, v, o.JQ)
	}
	switch o.Format {
	case FormatText:
		return writeText(w, v)
	case FormatID:
		return writeIDs(w, v)
	default:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}
}

func writeJQ(w io.Writer, v any, expr string) error {
	q, err := gojq.Parse(expr)
	if err != nil {
		return errs.Usage("--jq: %v", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return err
	}
	iter := q.Run(generic)
	for {
		r, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, ok := r.(error); ok {
			if _, halt := err.(*gojq.HaltError); halt {
				return nil
			}
			return errs.Usage("--jq: %v", err)
		}
		if s, ok := r.(string); ok {
			fmt.Fprintln(w, s)
			continue
		}
		out, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(out))
	}
}

func writeIDs(w io.Writer, v any) error {
	var ids []string
	switch x := v.(type) {
	case *provider.Message:
		ids = []string{x.ID}
	case *provider.Page:
		for _, m := range x.Messages {
			ids = append(ids, m.ID)
		}
	case []provider.Message:
		for _, m := range x {
			ids = append(ids, m.ID)
		}
	case []provider.AttachmentMeta:
		for _, a := range x {
			ids = append(ids, a.ID)
		}
	case interface{ IDs() []string }:
		ids = x.IDs()
	default:
		return errs.Usage("--format id is not supported for this command")
	}
	for _, id := range ids {
		fmt.Fprintln(w, id)
	}
	return nil
}

// TextWriter is implemented by result types that know how to print themselves.
type TextWriter interface {
	WriteText(w io.Writer) error
}

func writeText(w io.Writer, v any) error {
	switch x := v.(type) {
	case *provider.Message:
		return MessageText(w, x)
	case *provider.Page:
		ListText(w, x.Messages)
		if x.NextCursor != "" {
			fmt.Fprintf(w, "\nnext cursor: %s\n", x.NextCursor)
		}
		return nil
	case []provider.Message:
		for i := range x {
			if i > 0 {
				fmt.Fprintln(w)
			}
			if err := MessageText(w, &x[i]); err != nil {
				return err
			}
		}
		return nil
	case []provider.AttachmentMeta:
		for _, a := range x {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", a.ID, Clean(a.Name), a.Size, a.MIME)
		}
		return nil
	case TextWriter:
		return x.WriteText(w)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ListText prints one line per message: id, date, from, flags, subject.
func ListText(w io.Writer, msgs []provider.Message) {
	for _, m := range msgs {
		flag := " "
		if m.Flags.Unread {
			flag = "*"
		}
		fmt.Fprintf(w, "%s %s  %s  %-30s  %s\n", flag, m.ID, m.Date.Format(time.DateTime),
			truncRunes(Clean(m.From.String()), 30), Clean(m.Subject))
	}
}

func MessageText(w io.Writer, m *provider.Message) error {
	fmt.Fprintf(w, "id: %s\n", m.ID)
	if m.ThreadID != "" {
		fmt.Fprintf(w, "thread: %s\n", m.ThreadID)
	}
	fmt.Fprintf(w, "from: %s\n", Clean(m.From.String()))
	for _, f := range []struct {
		k string
		v []provider.Address
	}{{"to", m.To}, {"cc", m.Cc}, {"bcc", m.Bcc}, {"reply-to", m.ReplyTo}} {
		if len(f.v) > 0 {
			fmt.Fprintf(w, "%s: %s\n", f.k, Clean(joinAddrs(f.v)))
		}
	}
	fmt.Fprintf(w, "date: %s\n", m.Date.Format(time.RFC3339))
	fmt.Fprintf(w, "subject: %s\n", Clean(m.Subject))
	if len(m.Labels) > 0 {
		fmt.Fprintf(w, "labels: %s\n", Clean(strings.Join(m.Labels, ", ")))
	}
	var flags []string
	for name, on := range map[string]bool{"unread": m.Flags.Unread, "draft": m.Flags.Draft, "flagged": m.Flags.Flagged} {
		if on {
			flags = append(flags, name)
		}
	}
	if len(flags) > 0 {
		fmt.Fprintf(w, "flags: %s\n", strings.Join(sorted(flags), ", "))
	}
	for _, a := range m.Attachments {
		fmt.Fprintf(w, "attachment: %s (%d bytes, %s) id=%s\n", Clean(a.Name), a.Size, a.MIME, a.ID)
	}
	if m.BodyHiddenTextRemoved {
		fmt.Fprintln(w, "note: hidden text was removed from the HTML body")
	}
	body := m.BodyText
	if body == "" && m.Snippet != "" {
		body = m.Snippet
	}
	if body != "" {
		fmt.Fprint(w, Fence(m.ID, body))
		if m.BodyTruncated {
			fmt.Fprintf(w, "[body truncated: %d characters total; use --max-body to read more]\n", m.BodyLength)
		}
	}
	if m.BodyHTML != "" {
		fmt.Fprint(w, fence("UNTRUSTED MESSAGE HTML", m.ID, m.BodyHTML))
	}
	return nil
}

// Fence wraps untrusted body text in BEGIN/END markers. Any marker-like line
// inside the body is defused so a message cannot close the fence early.
func Fence(id, body string) string {
	return fence("UNTRUSTED MESSAGE BODY", id, body)
}

func fence(kind, id, body string) string {
	body = Clean(body)
	body = strings.ReplaceAll(body, "----- END UNTRUSTED", "-----[defused] END UNTRUSTED")
	body = strings.ReplaceAll(body, "----- BEGIN UNTRUSTED", "-----[defused] BEGIN UNTRUSTED")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return fmt.Sprintf("----- BEGIN %s (id=%s) -----\n%s----- END %s -----\n", kind, id, body, kind)
}

// Clean strips control characters (terminal escape sequences, bidi
// overrides) from untrusted text, keeping newlines and tabs.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return -1
		case unicode.IsControl(r):
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi embedding/override/isolate
			return -1
		}
		return r
	}, s)
}

func joinAddrs(as []provider.Address) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func sorted(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}
