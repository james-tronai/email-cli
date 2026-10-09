package imap

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/provider"
)

// ---- move / label / mark ----

// writable locates a message for a change; drafts cannot be moved or
// labelled.
func (p *Provider) writable(ctx context.Context, id string) (location, error) {
	if err := p.requireDraftMode(); err != nil {
		return location{}, err
	}
	l, err := p.locate(ctx, id)
	if err != nil {
		return l, err
	}
	if drafts, _ := p.byRole(ctx, provider.LabelDrafts); drafts != "" {
		if _, isDraft, err := p.inDrafts(ctx, id, drafts); err != nil {
			return l, err
		} else if isDraft {
			return l, errs.Usage("%s is a draft; drafts cannot be moved or labelled", id)
		}
	}
	return l, nil
}

func (p *Provider) storeUID(ctx context.Context, l location, item, list string) error {
	c, err := p.selectBox(ctx, l.box, true)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, raw("UID STORE"), raw(strconv.FormatUint(uint64(l.uid), 10)), raw(item), raw(list))
	return err
}

// gmSystem are the Gmail system labels written as atoms; anything else,
// including a user label that starts with a backslash, is quoted.
var gmSystem = map[string]bool{`\Inbox`: true, `\Starred`: true, `\Important`: true, `\Sent`: true, `\Draft`: true}

// gmLabels formats labels as an X-GM-LABELS list.
func gmLabels(labels []string) string {
	parts := make([]string, len(labels))
	for i, l := range labels {
		if gmSystem[l] {
			parts[i] = l
		} else {
			parts[i] = string(q(encodeMUTF7(l)).(raw))
		}
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// reread returns the message after a change. On Gmail the id is stable;
// elsewhere a move gives the message a new id, passed as newID.
func (p *Provider) reread(ctx context.Context, id string) (*provider.Message, error) {
	p.forget(id)
	f, err := p.fetchOne(ctx, id, false)
	if err != nil {
		return nil, err
	}
	m := p.convert(ctx, f)
	m.Headers = nil
	return &m, nil
}

// Move works like Gmail's "Move to" on Gmail (archive removes the Inbox
// label, a user label replaces it, spam and trash move the message);
// elsewhere it moves the message to the mailbox, which changes its id.
func (p *Provider) Move(ctx context.Context, id, dest string) (*provider.Message, error) {
	l, err := p.writable(ctx, id)
	if err != nil {
		return nil, err
	}
	low := strings.ToLower(dest)
	if low == provider.LabelSent || low == provider.LabelDrafts {
		return nil, errs.Usage("messages cannot be moved to %s", dest)
	}
	if p.gmail {
		return p.gmailMove(ctx, id, l, low, dest)
	}
	target, err := p.labelBox(ctx, dest)
	if err != nil {
		return nil, err
	}
	if target == l.box {
		return p.reread(ctx, id)
	}
	newUID, err := p.moveUIDGet(ctx, l.box, l.uid, target)
	if err != nil {
		return nil, err
	}
	if newUID == 0 {
		return nil, errs.Provider("noCopyUID", fmt.Errorf("moved to %q, but the server did not report the new UID; list %s to find it", target, dest))
	}
	c, err := p.selectBox(ctx, target, false)
	if err != nil {
		return nil, err
	}
	return p.reread(ctx, uidID(target, c.uidValidity, newUID))
}

func (p *Provider) gmailMove(ctx context.Context, id string, l location, low, dest string) (*provider.Message, error) {
	trash, _ := p.byRole(ctx, provider.LabelTrash)
	spam, _ := p.byRole(ctx, provider.LabelSpam)
	all, err := p.mustRole(ctx, "all")
	if err != nil {
		return nil, err
	}
	switch low {
	case provider.LabelTrash, provider.LabelSpam:
		target := trash
		if low == provider.LabelSpam {
			target = spam
		}
		if target == "" {
			return nil, errs.Unsupported("no %s mailbox on this server", low)
		}
		if l.box != target {
			if err := p.moveUID(ctx, l.box, l.uid, target); err != nil {
				return nil, err
			}
		}
		return p.reread(ctx, id)
	}
	// Out of Trash or Spam first: those messages are not in All Mail.
	if l.box == trash || l.box == spam {
		if err := p.moveUID(ctx, l.box, l.uid, "INBOX"); err != nil {
			return nil, err
		}
		p.forget(id)
		if l, err = p.locate(ctx, id); err != nil {
			return nil, err
		}
		if low == provider.LabelInbox {
			return p.reread(ctx, id)
		}
	}
	if l.box != all {
		// The message is found in All Mail unless it only sits in Drafts.
		return nil, errs.Usage("%s cannot be moved", id)
	}
	switch low {
	case provider.LabelArchive:
		err = p.storeUID(ctx, l, "-X-GM-LABELS.SILENT", gmLabels([]string{`\Inbox`}))
	case provider.LabelInbox:
		err = p.storeUID(ctx, l, "+X-GM-LABELS.SILENT", gmLabels([]string{`\Inbox`}))
	default:
		if err = p.ensureLabel(ctx, dest); err == nil {
			if err = p.storeUID(ctx, l, "+X-GM-LABELS.SILENT", gmLabels([]string{dest})); err == nil {
				err = p.storeUID(ctx, l, "-X-GM-LABELS.SILENT", gmLabels([]string{`\Inbox`}))
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return p.reread(ctx, id)
}

// ensureLabel creates a Gmail label (a mailbox) when it does not exist.
func (p *Provider) ensureLabel(ctx context.Context, name string) error {
	if _, ok, err := p.findBox(ctx, name); err != nil || ok {
		return err
	}
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	if _, err := c.run(ctx, raw("CREATE"), q(encodeMUTF7(name))); err != nil {
		return err
	}
	p.boxes = nil
	return nil
}

// Relabel adds and removes Gmail labels (X-GM-LABELS). Other servers have
// folders, not labels: use move.
func (p *Provider) Relabel(ctx context.Context, id string, add, remove []string) (*provider.Message, error) {
	if _, err := p.conn(ctx); err != nil {
		return nil, err
	}
	if !p.gmail {
		return nil, errs.Unsupported("this IMAP server has folders, not labels; use `email move`")
	}
	l, err := p.writable(ctx, id)
	if err != nil {
		return nil, err
	}
	all, err := p.mustRole(ctx, "all")
	if err != nil {
		return nil, err
	}
	if l.box != all {
		return nil, errs.Usage("%s is in trash or spam; move it out before labelling", id)
	}
	sys := func(lb string) (string, bool, error) {
		switch strings.ToLower(lb) {
		case provider.LabelInbox:
			return `\Inbox`, true, nil
		case provider.LabelArchive:
			return "", true, errs.Usage("archive is not a label; use `email move --to archive`")
		case provider.LabelTrash, provider.LabelSpam:
			return "", true, errs.Usage("use `email move --to %s`", strings.ToLower(lb))
		case provider.LabelSent, provider.LabelDrafts:
			return "", true, errs.Usage("Gmail does not allow changing the %s label", lb)
		}
		return "", false, nil
	}
	var addL, rmL []string
	for _, lb := range add {
		s, ok, err := sys(lb)
		if err != nil {
			return nil, err
		}
		if !ok {
			if err := p.ensureLabel(ctx, lb); err != nil {
				return nil, err
			}
			s = lb
		}
		addL = append(addL, s)
	}
	for _, lb := range remove {
		s, ok, err := sys(lb)
		if err != nil {
			return nil, err
		}
		if !ok {
			if _, found, err := p.findBox(ctx, lb); err != nil {
				return nil, err
			} else if !found {
				continue // nothing to remove
			}
			s = lb
		}
		rmL = append(rmL, s)
	}
	if len(addL) > 0 {
		if err := p.storeUID(ctx, l, "+X-GM-LABELS.SILENT", gmLabels(addL)); err != nil {
			return nil, err
		}
	}
	if len(rmL) > 0 {
		if err := p.storeUID(ctx, l, "-X-GM-LABELS.SILENT", gmLabels(rmL)); err != nil {
			return nil, err
		}
	}
	return p.reread(ctx, id)
}

// Mark sets \Seen and \Flagged (Gmail's star).
func (p *Provider) Mark(ctx context.Context, id string, o provider.MarkOptions) (*provider.Message, error) {
	if err := p.requireDraftMode(); err != nil {
		return nil, err
	}
	l, err := p.locate(ctx, id)
	if err != nil {
		return nil, err
	}
	set := func(on bool, flag string) error {
		op := "-FLAGS.SILENT"
		if on {
			op = "+FLAGS.SILENT"
		}
		return p.storeUID(ctx, l, op, "("+flag+")")
	}
	if o.Read != nil {
		if err := set(*o.Read, `\Seen`); err != nil {
			return nil, err
		}
	}
	if o.Flagged != nil {
		if err := set(*o.Flagged, `\Flagged`); err != nil {
			return nil, err
		}
	}
	return p.reread(ctx, id)
}

// ---- sync ----

// syncState is the sync cursor: the mailbox's UIDVALIDITY, the UIDNEXT
// after the last run, and HIGHESTMODSEQ when the server has CONDSTORE.
// New mail is every UID at or above UIDNEXT; an unchanged HIGHESTMODSEQ
// and UIDNEXT mean nothing happened and nothing is fetched. A changed
// UIDVALIDITY invalidates all UIDs: a new baseline is recorded and
// reported as initialized, as for an expired Gmail history id.
type syncState struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	UIDNext     uint32 `json:"uid_next"`
	ModSeq      uint64 `json:"highest_modseq,omitempty"`
}

func (p *Provider) status(ctx context.Context, box string) (syncState, error) {
	c, err := p.conn(ctx)
	if err != nil {
		return syncState{}, err
	}
	items := "(UIDVALIDITY UIDNEXT"
	if c.has("CONDSTORE") {
		items += " HIGHESTMODSEQ"
	}
	items += ")"
	rs, err := c.run(ctx, raw("STATUS"), q(encodeMUTF7(box)), raw(items))
	if err != nil {
		return syncState{}, err
	}
	st := syncState{Mailbox: box}
	for _, r := range rs {
		if r.Kind != "STATUS" || len(r.Fields) < 2 {
			continue
		}
		l := asList(r.Fields[1])
		for i := 0; i+1 < len(l); i += 2 {
			switch strings.ToUpper(asString(l[i])) {
			case "UIDVALIDITY":
				st.UIDValidity = uint32(asUint(l[i+1]))
			case "UIDNEXT":
				st.UIDNext = uint32(asUint(l[i+1]))
			case "HIGHESTMODSEQ":
				st.ModSeq = asUint(l[i+1])
			}
		}
	}
	if st.UIDValidity == 0 {
		return st, errs.Provider("invalidResponse", fmt.Errorf("STATUS %q returned no UIDVALIDITY", box))
	}
	return st, nil
}

// Idle waits on INBOX with IMAP IDLE until new mail arrives (true),
// maxWait passes or ctx ends (false). It changes nothing: INBOX is opened
// with EXAMINE. A server without IDLE gives errs.Unsupported. After any
// other error the connection is gone; Close the provider and open a new
// one.
func (p *Provider) Idle(ctx context.Context, maxWait time.Duration) (bool, error) {
	c, err := p.conn(ctx)
	if err != nil {
		return false, err
	}
	if !c.has("IDLE") {
		return false, errs.Unsupported("the IMAP server does not support IDLE")
	}
	if c, err = p.selectBox(ctx, "INBOX", false); err != nil {
		return false, err
	}
	if c.newMail {
		// Reported during the last sync, possibly after its STATUS.
		c.newMail = false
		return true, nil
	}
	woke, err := c.idle(ctx, maxWait)
	c.newMail = false
	return woke, err
}

func (p *Provider) Sync(ctx context.Context, o provider.SyncOptions) (*provider.SyncResult, error) {
	label := o.Label
	if label == "" {
		label = provider.LabelInbox
	}
	box, err := p.labelBox(ctx, label)
	if err != nil {
		return nil, err
	}
	var prev syncState
	if o.State != "" {
		if err := json.Unmarshal([]byte(o.State), &prev); err != nil || prev.UIDValidity == 0 {
			return nil, errs.Usage("sync state is not an imap cursor; delete the state file to start over")
		}
		if prev.Mailbox != box {
			return nil, errs.Usage("sync state is for mailbox %s, not %s; use another --state-file", prev.Mailbox, box)
		}
	}
	// Open the mailbox before reading its status: from here on the server
	// reports every arrival on this connection (see Idle).
	c, err := p.selectBox(ctx, box, false)
	if err != nil {
		return nil, err
	}
	c.newMail = false
	cur, err := p.status(ctx, box)
	if err != nil {
		return nil, err
	}
	if prev.UIDValidity == 0 || prev.UIDValidity != cur.UIDValidity {
		b, _ := json.Marshal(cur)
		return &provider.SyncResult{Messages: []provider.Message{}, State: string(b), Initialized: true}, nil
	}
	msgs := []provider.Message{}
	unchanged := cur.UIDNext == prev.UIDNext && (cur.ModSeq != 0 && cur.ModSeq == prev.ModSeq)
	next := max(cur.UIDNext, prev.UIDNext)
	if !unchanged && prev.UIDNext > 0 {
		uids, err := p.search(ctx, box, raw("UID"), raw(strconv.FormatUint(uint64(prev.UIDNext), 10)+":*"))
		if err != nil {
			return nil, err
		}
		// "n:*" also matches the highest UID when it is below n.
		uids = slices.DeleteFunc(uids, func(u uint32) bool { return u < prev.UIDNext })
		if len(uids) > 0 {
			next = max(next, uids[len(uids)-1]+1)
		}
		fs, err := p.fetch(ctx, box, uids, false)
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			m := p.convert(ctx, f)
			if m.Flags.Draft {
				continue
			}
			m.Headers = nil
			msgs = append(msgs, m)
		}
		slices.SortStableFunc(msgs, func(a, b provider.Message) int { return a.Date.Compare(b.Date) })
	}
	cur.UIDNext = next
	b, _ := json.Marshal(cur)
	return &provider.SyncResult{Messages: msgs, State: string(b)}, nil
}
