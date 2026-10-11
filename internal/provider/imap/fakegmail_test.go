package imap

import (
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/james-tronai/email-cli/internal/mailmime"
)

// fakeGmail is an in-memory IMAP server with Gmail's model: one store of
// messages with labels, shown as mailboxes (INBOX, [Gmail]/All Mail, label
// folders…), each with its own UIDs, and the X-GM-EXT-1 extensions. It
// implements what the provider uses, the way Gmail documents it.
type fakeGmail struct {
	t    *testing.T
	mu   sync.Mutex
	user string
	pass string

	msgs      []*gmMsg
	boxes     map[string]*gmBox
	labels    []string // user labels
	nextMsgID uint64
	modseq    uint64

	conns, active, maxActive int
	log                      []string // commands, without LOGIN arguments
	rawQueries               []string // X-GM-RAW strings received
}

type gmMsg struct {
	msgID, thrID uint64
	raw          []byte
	messageID    string
	labels       []string // \Inbox \Sent \Draft \Important, user labels
	flags        []string // \Seen \Flagged \Draft \Deleted
	trash, spam  bool
	date         time.Time
	modseq       uint64
}

type gmBox struct {
	name     string
	attr     string
	validity uint32
	uidNext  uint32
	uids     map[uint64]uint32 // msgID → UID
}

const (
	gmAll    = "[Gmail]/All Mail"
	gmDrafts = "[Gmail]/Drafts"
	gmSent   = "[Gmail]/Sent Mail"
	gmTrash  = "[Gmail]/Trash"
	gmSpam   = "[Gmail]/Spam"
	gmStar   = "[Gmail]/Starred"
)

func newFakeGmail(t *testing.T, user, pass string) *fakeGmail {
	f := &fakeGmail{t: t, user: user, pass: pass, boxes: map[string]*gmBox{}, nextMsgID: 0x18f0000000000001}
	for name, attr := range map[string]string{"INBOX": "", gmAll: `\All`, gmDrafts: `\Drafts`, gmSent: `\Sent`,
		gmTrash: `\Trash`, gmSpam: `\Junk`, gmStar: `\Flagged`} {
		f.boxes[name] = &gmBox{name: name, attr: attr, validity: 11, uidNext: 1, uids: map[uint64]uint32{}}
	}
	return f
}

// start serves implicit TLS on a random port.
func (f *fakeGmail) start(serverTLS *tls.Config) (string, int) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(nc)
		}
	}()
	return hostPort(f.t, ln.Addr())
}

func (f *fakeGmail) addLabel(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLabelLocked(name)
}

func (f *fakeGmail) ensureLabelLocked(name string) {
	if _, ok := f.boxes[name]; ok {
		return
	}
	f.labels = append(f.labels, name)
	f.boxes[name] = &gmBox{name: name, validity: 11, uidNext: 1, uids: map[uint64]uint32{}}
}

// deliver adds a message as if it arrived (labels default to \Inbox).
func (f *fakeGmail) deliver(raw string, labels ...string) *gmMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if labels == nil {
		labels = []string{`\Inbox`}
	}
	return f.addLocked([]byte(strings.ReplaceAll(raw, "\n", "\r\n")), labels, nil)
}

func (f *fakeGmail) addLocked(raw []byte, labels, flags []string) *gmMsg {
	f.modseq++
	m := &gmMsg{msgID: f.nextMsgID, raw: raw, labels: labels, flags: flags, date: time.Now().UTC().Truncate(time.Second), modseq: f.modseq}
	f.nextMsgID += 0x10
	h := mailmime.Parse(raw).Header
	m.messageID = strings.TrimSpace(h.Get("Message-Id"))
	m.thrID = m.msgID
	refs := strings.Fields(h.Get("References") + " " + h.Get("In-Reply-To"))
	for _, o := range f.msgs {
		if o.messageID != "" && slices.Contains(refs, o.messageID) {
			m.thrID = o.thrID
			break
		}
	}
	if d, err := time.Parse(time.RFC1123Z, h.Get("Date")); err == nil {
		m.date = d.UTC()
	}
	f.msgs = append(f.msgs, m)
	return m
}

func (f *fakeGmail) byMsgID(id uint64) *gmMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.msgs {
		if m.msgID == id {
			return m
		}
	}
	return nil
}

func (f *fakeGmail) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeGmail) member(box *gmBox, m *gmMsg) bool {
	has := func(l string) bool { return slices.Contains(m.labels, l) }
	switch box.name {
	case gmAll:
		return !m.trash && !m.spam
	case "INBOX":
		return has(`\Inbox`) && !m.trash && !m.spam
	case gmDrafts:
		return has(`\Draft`) && !m.trash
	case gmSent:
		return has(`\Sent`) && !m.trash
	case gmTrash:
		return m.trash
	case gmSpam:
		return m.spam
	case gmStar:
		return slices.Contains(m.flags, `\Flagged`) && !m.trash
	}
	return has(box.name) && !m.trash && !m.spam
}

// view returns a mailbox's messages in UID order, assigning UIDs to new
// members and dropping those that left (Gmail gives a returning message a
// new UID).
func (f *fakeGmail) view(box *gmBox) ([]*gmMsg, []uint32) {
	in := map[uint64]bool{}
	for _, m := range f.msgs {
		if !f.member(box, m) {
			continue
		}
		in[m.msgID] = true
		if _, ok := box.uids[m.msgID]; !ok {
			box.uids[m.msgID] = box.uidNext
			box.uidNext++
		}
	}
	for id := range box.uids {
		if !in[id] {
			delete(box.uids, id)
		}
	}
	var msgs []*gmMsg
	for _, m := range f.msgs {
		if in[m.msgID] {
			msgs = append(msgs, m)
		}
	}
	slices.SortFunc(msgs, func(a, b *gmMsg) int { return int(box.uids[a.msgID]) - int(box.uids[b.msgID]) })
	uids := make([]uint32, len(msgs))
	for i, m := range msgs {
		uids[i] = box.uids[m.msgID]
	}
	return msgs, uids
}

// ---- protocol ----

const gmCaps = "IMAP4rev1 UNSELECT IDLE NAMESPACE QUOTA ID XLIST CHILDREN X-GM-EXT-1 UIDPLUS COMPRESS=DEFLATE ENABLE MOVE CONDSTORE ESEARCH UTF8=ACCEPT LIST-EXTENDED LIST-STATUS LITERAL- SPECIAL-USE APPENDLIMIT=35651584"

type gmSession struct {
	f        *fakeGmail
	c        *conn
	authed   bool
	selected *gmBox
	readOnly bool
	// told is the message count this session last reported (EXISTS).
	told int
	// clientID is what the client sent with ID.
	clientID []any
}

func (f *fakeGmail) serve(nc net.Conn) {
	f.mu.Lock()
	f.conns++
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
		nc.Close()
	}()
	s := &gmSession{f: f, c: newConn(nc)}
	s.c.onLiteral = func() { s.c.w.WriteString("+ go ahead\r\n"); s.c.w.Flush() }
	s.c.w.WriteString("* OK Gimap ready for requests from 127.0.0.1\r\n")
	s.c.w.Flush()
	for {
		tag, err := s.c.readWord()
		if err != nil {
			return
		}
		fields, err := s.c.readFields()
		if err != nil || len(fields) == 0 {
			return
		}
		f.mu.Lock()
		cmd := strings.ToUpper(asString(fields[0]))
		if cmd == "UID" && len(fields) > 1 {
			cmd += " " + strings.ToUpper(asString(fields[1]))
			fields = fields[1:]
		}
		if cmd == "LOGIN" {
			f.log = append(f.log, "LOGIN")
		} else {
			f.log = append(f.log, cmd)
		}
		if cmd == "IDLE" && s.selected != nil {
			f.mu.Unlock()
			s.idle()
			fmt.Fprintf(s.c.w, "%s OK IDLE terminated (Success)\r\n", tag)
			s.c.w.Flush()
			continue
		}
		status, text := s.handle(cmd, fields[1:])
		f.mu.Unlock()
		fmt.Fprintf(s.c.w, "%s %s %s\r\n", tag, status, text)
		s.c.w.Flush()
		if cmd == "LOGOUT" {
			return
		}
	}
}

// idle reports EXISTS when the selected mailbox grows, until DONE.
func (s *gmSession) idle() {
	count := func() int {
		s.f.mu.Lock()
		defer s.f.mu.Unlock()
		msgs, _ := s.f.view(s.selected)
		return len(msgs)
	}
	// Like Gmail, mail that arrived since the session was last told about
	// the mailbox is reported as soon as IDLE starts.
	n := s.told
	s.c.w.WriteString("+ idling\r\n")
	s.c.w.Flush()
	done := make(chan struct{})
	go func() {
		line, _ := s.c.readLine()
		if !strings.EqualFold(strings.TrimSpace(line), "DONE") {
			s.f.t.Errorf("IDLE ended with %q", line)
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		case <-time.After(10 * time.Millisecond):
		}
		if m := count(); m > n {
			n = m
			s.told = n
			s.out("* %d EXISTS", n)
			s.c.w.Flush()
		}
	}
}

func (s *gmSession) out(format string, args ...any) { fmt.Fprintf(s.c.w, format+"\r\n", args...) }

func (s *gmSession) handle(cmd string, args []any) (string, string) {
	f := s.f
	if !s.authed && cmd != "CAPABILITY" && cmd != "LOGIN" && cmd != "LOGOUT" {
		return "BAD", "not authenticated"
	}
	switch cmd {
	case "CAPABILITY":
		s.out("* CAPABILITY %s", gmCaps)
		return "OK", "Thats all she wrote!"
	case "LOGIN":
		if len(args) == 2 && asString(args[1]) == "normal-google-password" {
			return "NO", "[ALERT] Application-specific password required: https://support.google.com/accounts/answer/185833 (Failure)"
		}
		if len(args) != 2 || asString(args[0]) != f.user || asString(args[1]) != f.pass {
			return "NO", "[AUTHENTICATIONFAILED] Invalid credentials (Failure)"
		}
		s.authed = true
		return "OK", "[CAPABILITY " + gmCaps + "] " + f.user + " authenticated (Success)"
	case "LOGOUT":
		s.out("* BYE LOGOUT Requested")
		return "OK", "73 good day (Success)"
	case "ID":
		s.clientID = args
		s.out(`* ID ("name" "GImap" "vendor" "Google, Inc." "support-url" "https://support.google.com/mail")`)
		return "OK", "Success"
	case "NOOP":
		return "OK", "Success"
	case "LIST":
		s.out(`* LIST (\HasNoChildren) "/" "INBOX"`)
		s.out(`* LIST (\HasChildren \Noselect) "/" "[Gmail]"`)
		for _, name := range []string{gmAll, gmDrafts, gmSent, gmSpam, gmStar, gmTrash} {
			s.out(`* LIST (\HasNoChildren %s) "/" "%s"`, f.boxes[name].attr, name)
		}
		for _, l := range f.labels {
			s.out(`* LIST (\HasNoChildren) "/" %s`, string(q(encodeMUTF7(l)).(raw)))
		}
		return "OK", "Success"
	case "CREATE":
		f.ensureLabelLocked(decodeMUTF7(asString(args[0])))
		return "OK", "Success"
	case "SELECT", "EXAMINE":
		box := f.boxes[decodeMUTF7(asString(args[0]))]
		if box == nil {
			return "NO", "[NONEXISTENT] Unknown Mailbox"
		}
		msgs, _ := f.view(box)
		s.selected, s.readOnly = box, cmd == "EXAMINE"
		s.out(`* FLAGS (\Answered \Flagged \Draft \Deleted \Seen)`)
		s.out("* OK [UIDVALIDITY %d] UIDs valid.", box.validity)
		s.out("* %d EXISTS", len(msgs))
		s.told = len(msgs)
		s.out("* OK [UIDNEXT %d] Predicted next UID.", box.uidNext)
		s.out("* OK [HIGHESTMODSEQ %d]", f.modseq)
		if s.readOnly {
			return "OK", "[READ-ONLY] " + box.name + " selected. (Success)"
		}
		return "OK", "[READ-WRITE] " + box.name + " selected. (Success)"
	case "STATUS":
		box := f.boxes[decodeMUTF7(asString(args[0]))]
		if box == nil {
			return "NO", "[NONEXISTENT] Unknown Mailbox"
		}
		f.view(box)
		s.out(`* STATUS %s (UIDVALIDITY %d UIDNEXT %d HIGHESTMODSEQ %d)`, string(q(box.name).(raw)), box.validity, box.uidNext, f.modseq)
		return "OK", "Success"
	case "APPEND":
		box := f.boxes[decodeMUTF7(asString(args[0]))]
		if box == nil {
			return "NO", "[TRYCREATE] Folder doesn't exist."
		}
		var flags []string
		for _, x := range asList(args[1]) {
			flags = append(flags, asString(x))
		}
		var labels []string
		switch box.name {
		case gmDrafts:
			labels = []string{`\Draft`}
		case gmSent:
			labels = []string{`\Sent`}
		case "INBOX":
			labels = []string{`\Inbox`}
		case gmAll:
		default:
			labels = []string{box.name}
		}
		m := f.addLocked(asBytes(args[len(args)-1]), labels, flags)
		f.view(box)
		return "OK", fmt.Sprintf("[APPENDUID %d %d] (Success)", box.validity, box.uids[m.msgID])
	}
	if s.selected == nil {
		return "BAD", "no mailbox selected"
	}
	switch cmd {
	case "UID SEARCH":
		res, err := s.search(args)
		if err != nil {
			return "BAD", err.Error()
		}
		parts := make([]string, len(res))
		for i, u := range res {
			parts[i] = strconv.Itoa(int(u))
		}
		s.out("* SEARCH %s", strings.Join(parts, " "))
		return "OK", "SEARCH completed (Success)"
	case "UID FETCH":
		return s.fetch(args)
	case "UID STORE":
		if s.readOnly {
			return "NO", "STORE attempt on READ-ONLY folder (Failure)"
		}
		return s.store(args)
	case "UID MOVE":
		if s.readOnly {
			return "NO", "MOVE attempt on READ-ONLY folder (Failure)"
		}
		return s.move(args)
	case "UID EXPUNGE":
		if s.readOnly {
			return "NO", "EXPUNGE attempt on READ-ONLY folder (Failure)"
		}
		return s.expunge(asString(args[0]))
	}
	return "BAD", "unknown command " + cmd
}

// inSet reports whether uid is in an IMAP UID set ("1:4,7", "5:*").
func inSet(set string, uid, maxUID uint32) bool {
	for _, r := range strings.Split(set, ",") {
		lo, hi, isRange := strings.Cut(r, ":")
		parse := func(s string) uint32 {
			if s == "*" {
				return maxUID
			}
			n, _ := strconv.Atoi(s)
			return uint32(n)
		}
		a := parse(lo)
		b := a
		if isRange {
			b = parse(hi)
		}
		if a > b {
			a, b = b, a
		}
		if uid >= a && uid <= b {
			return true
		}
	}
	return false
}

func (s *gmSession) search(args []any) ([]uint32, error) {
	f := s.f
	msgs, uids := f.view(s.selected)
	maxUID := uint32(0)
	if len(uids) > 0 {
		maxUID = uids[len(uids)-1]
	}
	type pred func(m *gmMsg, uid uint32) bool
	var preds []pred
	not := false
	add := func(p pred) {
		if not {
			inner := p
			p = func(m *gmMsg, uid uint32) bool { return !inner(m, uid) }
			not = false
		}
		preds = append(preds, p)
	}
	for i := 0; i < len(args); i++ {
		key := strings.ToUpper(asString(args[i]))
		arg := func() string { i++; return asString(args[i]) }
		switch key {
		case "CHARSET":
			arg()
		case "ALL":
		case "NOT":
			not = true
		case "UNSEEN":
			add(func(m *gmMsg, _ uint32) bool { return !slices.Contains(m.flags, `\Seen`) })
		case "DELETED":
			add(func(m *gmMsg, _ uint32) bool { return slices.Contains(m.flags, `\Deleted`) })
		case "UID":
			set := arg()
			add(func(_ *gmMsg, uid uint32) bool { return inSet(set, uid, maxUID) })
		case "X-GM-MSGID":
			n, _ := strconv.ParseUint(arg(), 10, 64)
			add(func(m *gmMsg, _ uint32) bool { return m.msgID == n })
		case "X-GM-THRID":
			n, _ := strconv.ParseUint(arg(), 10, 64)
			add(func(m *gmMsg, _ uint32) bool { return m.thrID == n })
		case "HEADER":
			name, val := arg(), arg()
			add(func(m *gmMsg, _ uint32) bool {
				return strings.Contains(mailmime.Parse(m.raw).Header.Get(name), val)
			})
		case "X-GM-RAW":
			qs := arg()
			f.rawQueries = append(f.rawQueries, qs)
			add(func(m *gmMsg, _ uint32) bool { return gmRawMatch(qs, m) })
		default:
			return nil, fmt.Errorf("unsupported search key %s", key)
		}
	}
	var out []uint32
	for i, m := range msgs {
		ok := true
		for _, p := range preds {
			ok = ok && p(m, uids[i])
		}
		if ok {
			out = append(out, uids[i])
		}
	}
	return out, nil
}

// gmRawMatch understands enough Gmail search syntax for the tests.
func gmRawMatch(qs string, m *gmMsg) bool {
	h := mailmime.Parse(m.raw).Header
	for _, tok := range strings.Fields(qs) {
		neg := strings.HasPrefix(tok, "-")
		tok = strings.TrimPrefix(tok, "-")
		k, v, _ := strings.Cut(tok, ":")
		var hit bool
		switch k {
		case "in":
			switch v {
			case "inbox":
				hit = slices.Contains(m.labels, `\Inbox`)
			case "sent":
				hit = slices.Contains(m.labels, `\Sent`)
			case "drafts":
				hit = slices.Contains(m.labels, `\Draft`)
			case "spam":
				hit = m.spam
			case "trash":
				hit = m.trash
			}
		case "is":
			hit = v == "unread" && !slices.Contains(m.flags, `\Seen`)
		case "from":
			hit = strings.Contains(strings.ToLower(h.Get("From")), strings.ToLower(v))
		case "after":
			n, _ := strconv.ParseInt(v, 10, 64)
			hit = m.date.Unix() > n
		case "label":
			hit = slices.Contains(m.labels, v)
		default:
			hit = strings.Contains(strings.ToLower(mailmime.DecodeHeader(h.Get("Subject"))), strings.ToLower(tok))
		}
		if hit == neg {
			return false
		}
	}
	return true
}

func (s *gmSession) fetch(args []any) (string, string) {
	f := s.f
	set := asString(args[0])
	var items []string
	if l, ok := args[1].(vlist); ok {
		for _, x := range l {
			items = append(items, asString(x))
		}
	} else {
		items = []string{asString(args[1])}
	}
	msgs, uids := f.view(s.selected)
	maxUID := uint32(0)
	if len(uids) > 0 {
		maxUID = uids[len(uids)-1]
	}
	for i, m := range msgs {
		if !inSet(set, uids[i], maxUID) {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "* %d FETCH (UID %d", i+1, uids[i])
		for _, it := range items {
			up := strings.ToUpper(it)
			switch {
			case up == "UID":
			case up == "FLAGS":
				fmt.Fprintf(&b, " FLAGS (%s)", strings.Join(m.flags, " "))
			case up == "INTERNALDATE":
				fmt.Fprintf(&b, ` INTERNALDATE "%s"`, m.date.Format("02-Jan-2006 15:04:05 -0700"))
			case up == "RFC822.SIZE":
				fmt.Fprintf(&b, " RFC822.SIZE %d", len(m.raw))
			case up == "X-GM-MSGID":
				fmt.Fprintf(&b, " X-GM-MSGID %d", m.msgID)
			case up == "X-GM-THRID":
				fmt.Fprintf(&b, " X-GM-THRID %d", m.thrID)
			case up == "X-GM-LABELS":
				fmt.Fprintf(&b, " X-GM-LABELS %s", gmLabels(m.labels))
			case up == "BODY.PEEK[]" || up == "BODY[]":
				fmt.Fprintf(&b, " BODY[] {%d}\r\n%s", len(m.raw), m.raw)
				if up == "BODY[]" && !slices.Contains(m.flags, `\Seen`) {
					m.flags = append(m.flags, `\Seen`)
				}
			case strings.HasPrefix(up, "BODY.PEEK[HEADER"):
				hdr := m.raw
				if i := strings.Index(string(hdr), "\r\n\r\n"); i >= 0 {
					hdr = hdr[:i+4]
				}
				fmt.Fprintf(&b, " %s {%d}\r\n%s", strings.Replace(it, ".PEEK", "", 1), len(hdr), hdr)
			default:
				return "BAD", "unsupported fetch item " + it
			}
		}
		b.WriteString(")")
		s.out("%s", b.String())
	}
	return "OK", "Success"
}

func (s *gmSession) targets(set string) []*gmMsg {
	msgs, uids := s.f.view(s.selected)
	maxUID := uint32(0)
	if len(uids) > 0 {
		maxUID = uids[len(uids)-1]
	}
	var out []*gmMsg
	for i, m := range msgs {
		if inSet(set, uids[i], maxUID) {
			out = append(out, m)
		}
	}
	return out
}

func (s *gmSession) store(args []any) (string, string) {
	f := s.f
	op := strings.ToUpper(asString(args[1]))
	var vals []string
	for _, x := range asList(args[2]) {
		vals = append(vals, decodeMUTF7(asString(x)))
	}
	for _, m := range s.targets(asString(args[0])) {
		f.modseq++
		m.modseq = f.modseq
		labels := strings.Contains(op, "X-GM-LABELS")
		list := &m.flags
		if labels {
			list = &m.labels
		}
		for _, v := range vals {
			if labels && !strings.HasPrefix(v, `\`) {
				f.ensureLabelLocked(v)
			}
			if strings.HasPrefix(op, "+") {
				if !slices.Contains(*list, v) {
					*list = append(*list, v)
				}
			} else {
				*list = slices.DeleteFunc(*list, func(x string) bool { return strings.EqualFold(x, v) })
			}
		}
	}
	return "OK", "Success"
}

func (s *gmSession) move(args []any) (string, string) {
	f := s.f
	dest := f.boxes[decodeMUTF7(asString(args[1]))]
	if dest == nil {
		return "NO", "[TRYCREATE] no such mailbox"
	}
	src := s.selected
	_, srcUIDs := f.view(src)
	var from, to []string
	for _, m := range s.targets(asString(args[0])) {
		from = append(from, strconv.Itoa(int(src.uids[m.msgID])))
		f.modseq++
		m.modseq = f.modseq
		switch src.name {
		case "INBOX":
			m.labels = slices.DeleteFunc(m.labels, func(l string) bool { return l == `\Inbox` })
		case gmTrash:
			m.trash = false
		case gmSpam:
			m.spam = false
		case gmAll, gmDrafts, gmSent, gmStar:
		default:
			m.labels = slices.DeleteFunc(m.labels, func(l string) bool { return l == src.name })
		}
		switch dest.name {
		case gmTrash:
			m.trash = true
		case gmSpam:
			m.spam = true
		case "INBOX":
			m.labels = append(m.labels, `\Inbox`)
		case gmAll:
		default:
			m.labels = append(m.labels, dest.name)
		}
		f.view(dest)
		to = append(to, strconv.Itoa(int(dest.uids[m.msgID])))
	}
	_ = srcUIDs
	s.out("* OK [COPYUID %d %s %s] (Success)", dest.validity, strings.Join(from, ","), strings.Join(to, ","))
	return "OK", "Success"
}

func (s *gmSession) expunge(set string) (string, string) {
	f := s.f
	for _, m := range s.targets(set) {
		if !slices.Contains(m.flags, `\Deleted`) {
			continue
		}
		f.modseq++
		switch s.selected.name {
		case gmTrash, gmSpam:
			f.msgs = slices.DeleteFunc(f.msgs, func(x *gmMsg) bool { return x == m }) // deleted forever
		case "INBOX":
			m.labels = slices.DeleteFunc(m.labels, func(l string) bool { return l == `\Inbox` })
		default:
			m.labels = slices.DeleteFunc(m.labels, func(l string) bool { return l == s.selected.name })
		}
		m.flags = slices.DeleteFunc(m.flags, func(x string) bool { return x == `\Deleted` })
	}
	return "OK", "Success"
}
