package imap

// A small IMAP4rev1 client: just what this provider needs, with room for
// Gmail's X-GM-* extensions that general-purpose libraries do not parse.
// Responses are read into a generic tree (atoms, strings, lists) that the
// provider interprets.

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
)

// Value types in a parsed response.
type (
	atom  string // an atom (NIL is returned as nil)
	str   []byte // a quoted string or a literal
	vlist []any
)

// lit is a command argument sent as a literal.
type lit []byte

// raw is a command argument written verbatim (atoms, lists, sequence sets).
type raw string

// response is one server response. Status responses (tagged, and untagged
// OK/NO/BAD/BYE/PREAUTH) fill Status, Code, CodeArgs and Text; untagged data
// responses fill Num (for "* 3 FETCH"), Kind and Fields.
type response struct {
	Tag      string // "*", "+" or the command tag
	Status   string
	Code     string
	CodeArgs []string
	Text     string
	Num      uint32
	Kind     string
	Fields   []any
}

const maxLiteral = 64 << 20

type conn struct {
	nc  net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
	tag int

	// onLiteral runs before a literal's bytes are read (a server sends
	// its continuation request here; the test server uses it).
	onLiteral func()

	caps map[string]bool

	// Selected mailbox state.
	selected    string
	readOnly    bool
	uidValidity uint32
	uidNext     uint32
	modSeq      uint64
	exists      uint32
}

// STARTTLS ports; every other port speaks implicit TLS. Plain text is
// never used: the password must not cross the wire unencrypted.
var (
	imapStartTLSPorts = map[int]bool{143: true}
	smtpStartTLSPorts = map[int]bool{25: true, 587: true}
)

// dial connects with implicit TLS, or STARTTLS on port 143.
func dial(ctx context.Context, host string, port int, tlsCfg *tls.Config) (*conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	cfg := tlsConfig(tlsCfg, host)
	d := &net.Dialer{Timeout: 30 * time.Second}
	var nc net.Conn
	var err error
	if imapStartTLSPorts[port] {
		nc, err = d.DialContext(ctx, "tcp", addr)
	} else {
		nc, err = (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, netErr(ctx, "connect", err)
	}
	c := newConn(nc)
	stop := c.watch(ctx)
	defer stop()
	greet, err := c.readResponse()
	if err != nil {
		nc.Close()
		return nil, netErr(ctx, "greeting", err)
	}
	if greet.Status != "OK" && greet.Status != "PREAUTH" {
		nc.Close()
		return nil, errs.Provider("greeting", fmt.Errorf("IMAP server refused the connection: %s %s", greet.Status, greet.Text))
	}
	if greet.Code == "CAPABILITY" {
		c.setCaps(greet.CodeArgs)
	}
	if _, isTLS := nc.(*tls.Conn); !isTLS {
		if err := c.startTLS(ctx, cfg); err != nil {
			nc.Close()
			return nil, err
		}
	}
	return c, nil
}

func tlsConfig(base *tls.Config, host string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if base != nil {
		cfg = base.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	return cfg
}

func newConn(nc net.Conn) *conn {
	return &conn{nc: nc, r: bufio.NewReaderSize(nc, 64<<10), w: bufio.NewWriter(nc)}
}

func (c *conn) startTLS(ctx context.Context, cfg *tls.Config) error {
	if c.caps == nil {
		if err := c.capability(ctx); err != nil {
			return err
		}
	}
	if !c.caps["STARTTLS"] {
		return errs.Provider("noTLS", errors.New("the IMAP server offers no STARTTLS on this port; use port 993 (implicit TLS)"))
	}
	if _, err := c.run(ctx, raw("STARTTLS")); err != nil {
		return err
	}
	tc := tls.Client(c.nc, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return netErr(ctx, "starttls", err)
	}
	c.nc, c.r, c.w = tc, bufio.NewReaderSize(tc, 64<<10), bufio.NewWriter(tc)
	c.caps = nil // must be asked again after STARTTLS
	return nil
}

// watch makes blocking reads and writes return when ctx ends.
func (c *conn) watch(ctx context.Context) func() {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.nc.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(time.Unix(1, 0)) })
	return func() { stop() }
}

func netErr(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return errs.Timeout(ctx.Err())
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errs.Timeout(err)
	}
	return errs.Provider("network", fmt.Errorf("IMAP %s: %w", what, err))
}

func (c *conn) setCaps(list []string) {
	c.caps = map[string]bool{}
	for _, s := range list {
		c.caps[strings.ToUpper(s)] = true
	}
}

func (c *conn) capability(ctx context.Context) error {
	rs, err := c.run(ctx, raw("CAPABILITY"))
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Kind == "CAPABILITY" {
			var list []string
			for _, f := range r.Fields {
				list = append(list, asString(f))
			}
			c.setCaps(list)
		}
	}
	if c.caps == nil {
		c.caps = map[string]bool{}
	}
	return nil
}

func (c *conn) has(capName string) bool { return c.caps[capName] }

func (c *conn) close() {
	if c == nil || c.nc == nil {
		return
	}
	_ = c.nc.SetDeadline(time.Now().Add(5 * time.Second))
	c.tag++
	fmt.Fprintf(c.w, "x%d LOGOUT\r\n", c.tag)
	_ = c.w.Flush()
	// Read until the tagged reply or an error; the server closes after BYE.
	for range 10 {
		r, err := c.readResponse()
		if err != nil || r.Tag != "*" {
			break
		}
	}
	_ = c.nc.Close()
	c.nc = nil
}

// ---- writing commands ----

// q returns a string argument: quoted when it can be, else a literal.
func q(s string) any {
	if len(s) > 1000 {
		return lit(s)
	}
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 || b >= 0x7f {
			return lit(s)
		}
	}
	return raw(`"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`)
}

// run sends one command and collects its untagged responses. A tagged NO
// or BAD becomes an error.
func (c *conn) run(ctx context.Context, args ...any) ([]*response, error) {
	stop := c.watch(ctx)
	defer stop()
	c.tag++
	tag := "x" + strconv.Itoa(c.tag)
	var untagged []*response
	if _, err := c.w.WriteString(tag); err != nil {
		return nil, netErr(ctx, "write", err)
	}
	for _, a := range args {
		c.w.WriteByte(' ')
		switch v := a.(type) {
		case raw:
			c.w.WriteString(string(v))
		case lit:
			fmt.Fprintf(c.w, "{%d}\r\n", len(v))
			if err := c.w.Flush(); err != nil {
				return nil, netErr(ctx, "write", err)
			}
			// Wait for the continuation request.
			for {
				r, err := c.readResponse()
				if err != nil {
					return nil, netErr(ctx, "read", err)
				}
				if r.Tag == "+" {
					break
				}
				if r.Tag == tag {
					return nil, statusErr(r)
				}
				untagged = append(untagged, r)
			}
			c.w.Write(v)
		default:
			return nil, fmt.Errorf("imap: bad argument type %T", a)
		}
	}
	c.w.WriteString("\r\n")
	if err := c.w.Flush(); err != nil {
		return nil, netErr(ctx, "write", err)
	}
	for {
		r, err := c.readResponse()
		if err != nil {
			return nil, netErr(ctx, "read", err)
		}
		switch {
		case r.Tag == tag:
			if r.Status != "OK" {
				return untagged, statusErr(r)
			}
			// The tagged OK can carry data too (APPENDUID, COPYUID, READ-ONLY).
			return append(untagged, r), nil
		case r.Tag == "*" && r.Status == "BYE":
			return nil, errs.Provider("bye", fmt.Errorf("IMAP server closed the connection: %s", r.Text))
		case r.Tag == "*":
			untagged = append(untagged, r)
		}
	}
}

func statusErr(r *response) error {
	code := r.Code
	if code == "" {
		code = r.Status
	}
	msg := strings.TrimSpace(r.Text)
	if msg == "" {
		msg = r.Status
	}
	if r.Code == "AUTHENTICATIONFAILED" || r.Code == "AUTHORIZATIONFAILED" {
		return errs.Auth("IMAP login failed: %s", msg)
	}
	return errs.Provider(code, fmt.Errorf("IMAP: %s", msg))
}

// ---- reading responses ----

func (c *conn) readResponse() (*response, error) {
	tag, err := c.readWord()
	if err != nil {
		return nil, err
	}
	r := &response{Tag: tag}
	if tag == "+" {
		line, err := c.readLine()
		r.Text = strings.TrimSpace(line)
		return r, err
	}
	word, err := c.readWord()
	if err != nil {
		return nil, err
	}
	if tag != "*" {
		r.Status = strings.ToUpper(word)
		return r, c.readStatusRest(r)
	}
	if n, err := strconv.ParseUint(word, 10, 32); err == nil {
		r.Num = uint32(n)
		if word, err = c.readWord(); err != nil {
			return nil, err
		}
		r.Kind = strings.ToUpper(word)
		r.Fields, err = c.readFields()
		return r, err
	}
	switch up := strings.ToUpper(word); up {
	case "OK", "NO", "BAD", "BYE", "PREAUTH":
		r.Status = up
		return r, c.readStatusRest(r)
	default:
		r.Kind = up
		r.Fields, err = c.readFields()
		return r, err
	}
}

// readWord reads up to the next space or line end and consumes the space.
func (c *conn) readWord() (string, error) {
	var b strings.Builder
	for {
		ch, err := c.r.ReadByte()
		if err != nil {
			return "", err
		}
		switch ch {
		case ' ':
			return b.String(), nil
		case '\r', '\n':
			_ = c.r.UnreadByte()
			return b.String(), nil
		}
		b.WriteByte(ch)
	}
}

func (c *conn) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// readStatusRest parses "[CODE args] text" up to the line end.
func (c *conn) readStatusRest(r *response) error {
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if strings.HasPrefix(line, "[") {
		if i := strings.IndexByte(line, ']'); i > 0 {
			f := strings.Fields(line[1:i])
			if len(f) > 0 {
				r.Code, r.CodeArgs = strings.ToUpper(f[0]), f[1:]
				// CAPABILITY and PERMANENTFLAGS lists keep their atoms.
				for j, a := range r.CodeArgs {
					r.CodeArgs[j] = strings.Trim(a, "()")
				}
			}
			line = line[i+1:]
		}
	}
	r.Text = strings.TrimSpace(line)
	return nil
}

// readFields reads values until the end of the line.
func (c *conn) readFields() ([]any, error) {
	var out []any
	for {
		ch, err := c.r.ReadByte()
		if err != nil {
			return nil, err
		}
		switch ch {
		case ' ':
			continue
		case '\r':
			continue
		case '\n':
			return out, nil
		}
		_ = c.r.UnreadByte()
		v, err := c.readValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (c *conn) readValue() (any, error) {
	ch, err := c.r.ReadByte()
	if err != nil {
		return nil, err
	}
	switch ch {
	case '(':
		l := vlist{}
		for {
			ch, err := c.r.ReadByte()
			if err != nil {
				return nil, err
			}
			switch ch {
			case ')':
				return l, nil
			case ' ':
				continue
			case '\r', '\n':
				return nil, errors.New("imap: line ended inside a list")
			}
			_ = c.r.UnreadByte()
			v, err := c.readValue()
			if err != nil {
				return nil, err
			}
			l = append(l, v)
		}
	case '"':
		var b []byte
		for {
			ch, err := c.r.ReadByte()
			if err != nil {
				return nil, err
			}
			switch ch {
			case '"':
				return str(b), nil
			case '\\':
				if ch, err = c.r.ReadByte(); err != nil {
					return nil, err
				}
			case '\r', '\n':
				return nil, errors.New("imap: line ended inside a quoted string")
			}
			b = append(b, ch)
		}
	case '{', '~':
		if ch == '~' {
			if ch, err = c.r.ReadByte(); err != nil || ch != '{' {
				return nil, errors.New("imap: bad literal8")
			}
		}
		spec, err := c.r.ReadString('}')
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(spec, "}"), "+"))
		if err != nil || n < 0 || n > maxLiteral {
			return nil, fmt.Errorf("imap: bad literal size %q", spec)
		}
		if _, err := c.readLine(); err != nil { // the CRLF after {n}
			return nil, err
		}
		if c.onLiteral != nil && !strings.HasSuffix(spec, "+}") {
			c.onLiteral()
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(c.r, b); err != nil {
			return nil, err
		}
		return str(b), nil
	}
	// Atom. Brackets may hold spaces and parentheses: BODY[HEADER.FIELDS (A B)]<0>.
	_ = c.r.UnreadByte()
	var b strings.Builder
	depth := 0
	for {
		ch, err := c.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if depth == 0 && (ch == ' ' || ch == '(' || ch == ')' || ch == '\r' || ch == '\n') {
			_ = c.r.UnreadByte()
			break
		}
		switch ch {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '\r', '\n':
			return nil, errors.New("imap: line ended inside brackets")
		}
		b.WriteByte(ch)
	}
	s := b.String()
	if strings.EqualFold(s, "NIL") {
		return nil, nil
	}
	return atom(s), nil
}

// ---- value helpers ----

func asString(v any) string {
	switch x := v.(type) {
	case atom:
		return string(x)
	case str:
		return string(x)
	}
	return ""
}

func asBytes(v any) []byte {
	switch x := v.(type) {
	case str:
		return []byte(x)
	case atom:
		return []byte(x)
	}
	return nil
}

func asUint(v any) uint64 {
	n, _ := strconv.ParseUint(asString(v), 10, 64)
	return n
}

func asList(v any) vlist {
	l, _ := v.(vlist)
	return l
}

// fetchItems turns a FETCH response's list into name → value, with
// upper-cased names ("UID", "X-GM-MSGID", "BODY[HEADER]").
func fetchItems(r *response) map[string]any {
	out := map[string]any{}
	if len(r.Fields) == 0 {
		return out
	}
	l := asList(r.Fields[0])
	for i := 0; i+1 < len(l); i += 2 {
		out[strings.ToUpper(asString(l[i]))] = l[i+1]
	}
	return out
}
