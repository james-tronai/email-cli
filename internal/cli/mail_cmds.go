package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
	"github.com/james-tronai/email-cli/internal/render"
	"github.com/james-tronai/email-cli/internal/service"
)

type getFlags struct {
	includeHTML bool
	headers     bool
	maxBody     int
}

func (g *getFlags) register(c *cobra.Command) {
	c.Flags().BoolVar(&g.includeHTML, "include-html", false, "also return the raw HTML body (contains hidden text)")
	c.Flags().BoolVar(&g.headers, "headers", false, "include Message-ID, In-Reply-To, References, Authentication-Results")
	c.Flags().IntVar(&g.maxBody, "max-body", service.DefaultMaxBody, "truncate body_text to N characters (-1 = no limit)")
}

func (g *getFlags) opts() service.GetOptions {
	return service.GetOptions{IncludeHTML: g.includeHTML, Headers: g.headers, MaxBody: g.maxBody}
}

func (a *app) listCmd() *cobra.Command {
	var o provider.ListOptions
	var since string
	c := &cobra.Command{
		Use:   "list",
		Short: "List messages in a label/folder (newest first)",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if since != "" {
				t, err := time.Parse(time.RFC3339, since)
				if err != nil {
					return errs.Usage("--since must be RFC 3339, e.g. 2026-10-01T00:00:00Z")
				}
				o.Since = t
			}
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			pg, err := s.List(cmd.Context(), o)
			if err != nil {
				return err
			}
			return a.output(pg)
		},
	}
	c.Flags().StringVar(&o.Label, "label", "inbox", "label or folder")
	c.Flags().BoolVar(&o.Unread, "unread", false, "only unread messages")
	c.Flags().StringVar(&since, "since", "", "only messages received at or after this RFC 3339 time")
	c.Flags().IntVar(&o.Limit, "limit", 20, "page size")
	c.Flags().StringVar(&o.Cursor, "cursor", "", "next_cursor from a previous page")
	return c
}

func (a *app) searchCmd() *cobra.Command {
	var o provider.SearchOptions
	c := &cobra.Command{
		Use:   "search <query>",
		Short: "Search with the provider's native syntax (Graph KQL, Gmail q=)",
		Args:  exactArgs(1, "<query>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Query = args[0]
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			pg, err := s.Search(cmd.Context(), o)
			if err != nil {
				return err
			}
			return a.output(pg)
		},
	}
	c.Flags().IntVar(&o.Limit, "limit", 20, "page size")
	c.Flags().StringVar(&o.Cursor, "cursor", "", "next_cursor from a previous page")
	return c
}

func (a *app) getCmd() *cobra.Command {
	var g getFlags
	c := &cobra.Command{
		Use:   "get <id>",
		Short: "Get one message; body is sanitized text, hidden HTML text removed",
		Args:  exactArgs(1, "<id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			m, err := s.Get(cmd.Context(), args[0], g.opts())
			if err != nil {
				return err
			}
			return a.output(m)
		},
	}
	g.register(c)
	return c
}

func (a *app) threadCmd() *cobra.Command {
	var g getFlags
	var limit int
	c := &cobra.Command{
		Use:   "thread <thread-id>",
		Short: "Get the messages in a thread (oldest first)",
		Args:  exactArgs(1, "<thread-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			msgs, err := s.Thread(cmd.Context(), args[0], limit, g.opts())
			if err != nil {
				return err
			}
			if msgs == nil {
				msgs = []provider.Message{}
			}
			return a.output(threadOutput{ThreadID: args[0], Messages: msgs})
		},
	}
	g.register(c)
	c.Flags().IntVar(&limit, "limit", 0, "only the last N messages (0 = all)")
	return c
}

// threadOutput is an object like list's page, so `--jq .messages` works
// for both (v0.6.0; it used to be a bare array).
type threadOutput struct {
	ThreadID string             `json:"thread_id"`
	Messages []provider.Message `json:"messages"`
}

func (t threadOutput) IDs() []string {
	ids := make([]string, len(t.Messages))
	for i, m := range t.Messages {
		ids[i] = m.ID
	}
	return ids
}

func (t threadOutput) WriteText(w io.Writer) error {
	for i := range t.Messages {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if err := render.MessageText(w, &t.Messages[i]); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) attachmentsCmd() *cobra.Command {
	c := &cobra.Command{Use: "attachments", Short: "List or download attachments"}
	c.AddCommand(&cobra.Command{
		Use:   "list <message-id>",
		Short: "List attachment metadata",
		Args:  exactArgs(1, "<message-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			as, err := s.ListAttachments(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if as == nil {
				as = []provider.AttachmentMeta{}
			}
			return a.output(as)
		},
	})
	var aid, out string
	dl := &cobra.Command{
		Use:   "download <message-id> --out <dir>",
		Short: "Save attachments to a directory (sanitized names, 0600, never executed)",
		Args:  exactArgs(1, "<message-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.DownloadAttachments(cmd.Context(), args[0], aid, out)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	dl.Flags().StringVar(&aid, "attachment", "", "only this attachment id")
	dl.Flags().StringVar(&out, "out", "", "output directory (required)")
	_ = dl.MarkFlagRequired("out")
	c.AddCommand(dl)
	return c
}

type composeFlags struct {
	to, cc, bcc []string
	subject     string
	body        string
	bodyFile    string
	attach      []string
}

func (f *composeFlags) registerRecipients(c *cobra.Command) {
	c.Flags().StringArrayVar(&f.to, "to", nil, "recipient(s); repeat or comma-separate")
	c.Flags().StringArrayVar(&f.cc, "cc", nil, "cc recipient(s)")
	c.Flags().StringArrayVar(&f.bcc, "bcc", nil, "bcc recipient(s)")
}

func (f *composeFlags) registerBody(c *cobra.Command) {
	c.Flags().StringVar(&f.body, "body", "", "body text, or - to read stdin")
	c.Flags().StringVar(&f.bodyFile, "body-file", "", "read body text from file")
}

func (a *app) draftCmd() *cobra.Command {
	c := &cobra.Command{Use: "draft", Short: "Create, reply, forward, update, list and delete drafts"}
	c.AddCommand(a.draftCreateCmd(), a.draftReplyCmd(), a.draftForwardCmd(), a.draftUpdateCmd(),
		a.draftListCmd(), a.draftGetCmd(), a.draftDeleteCmd())
	return c
}

func (a *app) draftCreateCmd() *cobra.Command {
	var f composeFlags
	c := &cobra.Command{
		Use:   "create --to … --subject … (--body-file f | --body -)",
		Short: "Create a new draft (signoff appended per policy)",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := provider.DraftInput{Subject: f.subject}
			var err error
			if in.To, err = parseAddrs(f.to); err != nil {
				return err
			}
			if in.Cc, err = parseAddrs(f.cc); err != nil {
				return err
			}
			if in.Bcc, err = parseAddrs(f.bcc); err != nil {
				return err
			}
			if in.Body, _, err = a.readBody(f.body, f.bodyFile, true); err != nil {
				return err
			}
			if in.Attachments, err = readAttachments(f.attach); err != nil {
				return err
			}
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.CreateDraft(cmd.Context(), in)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	f.registerRecipients(c)
	f.registerBody(c)
	c.Flags().StringVar(&f.subject, "subject", "", "subject")
	c.Flags().StringArrayVar(&f.attach, "attach", nil, "file to attach (repeatable)")
	_ = c.MarkFlagRequired("subject")
	return c
}

func (a *app) draftReplyCmd() *cobra.Command {
	var f composeFlags
	var all bool
	c := &cobra.Command{
		Use:   "reply <message-id> (--body-file f | --body -)",
		Short: "Create a reply draft on the server (keeps thread headers)",
		Args:  exactArgs(1, "<message-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := provider.ReplyInput{MessageID: args[0], All: all}
			var err error
			if in.Body, _, err = a.readBody(f.body, f.bodyFile, true); err != nil {
				return err
			}
			if in.Attachments, err = readAttachments(f.attach); err != nil {
				return err
			}
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.ReplyDraft(cmd.Context(), in)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	f.registerBody(c)
	c.Flags().BoolVar(&all, "all", false, "reply to all recipients")
	c.Flags().StringArrayVar(&f.attach, "attach", nil, "file to attach (repeatable)")
	return c
}

func (a *app) draftForwardCmd() *cobra.Command {
	var f composeFlags
	c := &cobra.Command{
		Use:   "forward <message-id> --to … (--body-file f | --body -)",
		Short: "Create a forward draft (external recipients blocked by policy)",
		Args:  exactArgs(1, "<message-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := provider.ForwardInput{MessageID: args[0]}
			var err error
			if in.To, err = parseAddrs(f.to); err != nil {
				return err
			}
			if in.Body, _, err = a.readBody(f.body, f.bodyFile, false); err != nil {
				return err
			}
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.ForwardDraft(cmd.Context(), in)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	c.Flags().StringArrayVar(&f.to, "to", nil, "recipient(s)")
	f.registerBody(c)
	_ = c.MarkFlagRequired("to")
	return c
}

func (a *app) draftUpdateCmd() *cobra.Command {
	var f composeFlags
	c := &cobra.Command{
		Use:   "update <draft-id> [--to …] [--subject …] [--body …]",
		Short: "Change fields of an existing draft (only the flags given)",
		Args:  exactArgs(1, "<draft-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var u provider.DraftUpdate
			for _, x := range []struct {
				flag string
				vals []string
				dst  **[]provider.Address
			}{{"to", f.to, &u.To}, {"cc", f.cc, &u.Cc}, {"bcc", f.bcc, &u.Bcc}} {
				if cmd.Flags().Changed(x.flag) {
					list, err := parseAddrs(x.vals)
					if err != nil {
						return err
					}
					*x.dst = &list
				}
			}
			if cmd.Flags().Changed("subject") {
				u.Subject = &f.subject
			}
			body, ok, err := a.readBody(f.body, f.bodyFile, false)
			if err != nil {
				return err
			}
			if ok {
				u.Body = &body
			}
			if u.Attachments, err = readAttachments(f.attach); err != nil {
				return err
			}
			if u.To == nil && u.Cc == nil && u.Bcc == nil && u.Subject == nil && u.Body == nil && len(u.Attachments) == 0 {
				return errs.Usage("nothing to update")
			}
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.UpdateDraft(cmd.Context(), args[0], u)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	f.registerRecipients(c)
	f.registerBody(c)
	c.Flags().StringVar(&f.subject, "subject", "", "subject")
	c.Flags().StringArrayVar(&f.attach, "attach", nil, "file to attach (repeatable)")
	return c
}

func (a *app) draftListCmd() *cobra.Command {
	var o provider.ListOptions
	c := &cobra.Command{
		Use:   "list",
		Short: "List drafts",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			pg, err := s.ListDrafts(cmd.Context(), o)
			if err != nil {
				return err
			}
			return a.output(pg)
		},
	}
	c.Flags().IntVar(&o.Limit, "limit", 20, "page size")
	c.Flags().StringVar(&o.Cursor, "cursor", "", "next_cursor from a previous page")
	return c
}

func (a *app) draftGetCmd() *cobra.Command {
	var g getFlags
	c := &cobra.Command{
		Use:   "get <draft-id>",
		Short: "Get one draft",
		Args:  exactArgs(1, "<draft-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			m, err := s.GetDraft(cmd.Context(), args[0], g.opts())
			if err != nil {
				return err
			}
			return a.output(m)
		},
	}
	g.register(c)
	return c
}

func (a *app) draftDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <draft-id>",
		Short: "Delete a draft (drafts only; mail deletion is disabled)",
		Args:  exactArgs(1, "<draft-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.DeleteDraft(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
}

func (a *app) sendCmd() *cobra.Command {
	var key string
	c := &cobra.Command{
		Use:   "send <draft-id>",
		Short: "Send an existing draft (only in mode=send)",
		Args:  exactArgs(1, "<draft-id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.Send(cmd.Context(), args[0], key)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	c.Flags().StringVar(&key, "idempotency-key", "", "dedupe key (default: draft id)")
	return c
}

func (a *app) moveCmd() *cobra.Command {
	var dest string
	c := &cobra.Command{
		Use:   "move <id> --to archive|<label>",
		Short: "Move a message (Graph: folder move; Gmail: label change)",
		Args:  exactArgs(1, "<id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.Move(cmd.Context(), args[0], dest)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	c.Flags().StringVar(&dest, "to", "", "destination: inbox, archive, spam, or a label/folder name")
	_ = c.MarkFlagRequired("to")
	return c
}

func (a *app) labelCmd() *cobra.Command {
	var add, remove []string
	c := &cobra.Command{
		Use:   "label <id> [--add x] [--remove y]",
		Short: "Add or remove labels (Graph: categories)",
		Args:  exactArgs(1, "<id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.Label(cmd.Context(), args[0], add, remove)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	c.Flags().StringArrayVar(&add, "add", nil, "label to add (repeatable)")
	c.Flags().StringArrayVar(&remove, "remove", nil, "label to remove (repeatable)")
	return c
}

func (a *app) markCmd() *cobra.Command {
	var read, unread, flag, unflag bool
	c := &cobra.Command{
		Use:   "mark <id> --read|--unread|--flag|--unflag",
		Short: "Change read/flag state",
		Args:  exactArgs(1, "<id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var o provider.MarkOptions
			if read || unread {
				v := read
				o.Read = &v
			}
			if flag || unflag {
				v := flag
				o.Flagged = &v
			}
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.Mark(cmd.Context(), args[0], o)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	c.Flags().BoolVar(&read, "read", false, "mark read")
	c.Flags().BoolVar(&unread, "unread", false, "mark unread")
	c.Flags().BoolVar(&flag, "flag", false, "flag")
	c.Flags().BoolVar(&unflag, "unflag", false, "remove flag")
	c.MarkFlagsMutuallyExclusive("read", "unread")
	c.MarkFlagsMutuallyExclusive("flag", "unflag")
	return c
}

func (a *app) syncCmd() *cobra.Command {
	var label, stateFile string
	c := &cobra.Command{
		Use:   "sync [--label inbox] [--state-file path]",
		Short: "Print mail that arrived since the last sync (for cron); first run records a baseline",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.Sync(cmd.Context(), label, stateFile)
			if err != nil {
				return err
			}
			return a.output(res)
		},
	}
	c.Flags().StringVar(&label, "label", "inbox", "label or folder to watch")
	c.Flags().StringVar(&stateFile, "state-file", "", "cursor file (default: <state dir>/sync/<account>-<label>.cursor)")
	return c
}

func (a *app) apiCmd() *cobra.Command {
	var bodyFile string
	c := &cobra.Command{
		Use:   "api <METHOD> <path>",
		Short: "Call the provider API directly with this account's auth (gated by mode)",
		Long: `Escape hatch. Paths are relative to the provider base URL
(Graph: https://graph.microsoft.com/v1.0, Gmail: https://gmail.googleapis.com).
GET needs mode read; POST/PATCH/PUT need draft; anything that sends needs send;
DELETE needs [delete] enabled.`,
		Args: exactArgs(2, "<METHOD>", "<path>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var body []byte
			switch bodyFile {
			case "":
			case "-":
				b, err := io.ReadAll(a.stdin)
				if err != nil {
					return err
				}
				body = b
			default:
				b, err := os.ReadFile(bodyFile)
				if err != nil {
					return errs.Usage("--input: %v", err)
				}
				body = b
			}
			s, err := a.open(cmd.Context())
			if err != nil {
				return err
			}
			res, err := s.API(cmd.Context(), args[0], args[1], body)
			if res != nil {
				if oerr := a.output(res); oerr != nil && err == nil {
					err = oerr
				}
			}
			return err
		},
	}
	c.Flags().StringVar(&bodyFile, "input", "", "request body file (- for stdin)")
	return c
}
