package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/james-tronai/email-cli/internal/errs"
	"github.com/james-tronai/email-cli/internal/mailmime"
	"github.com/james-tronai/email-cli/internal/policy"
	"github.com/james-tronai/email-cli/internal/provider"
)

// Send submits a draft over SMTP and then removes it from Drafts. Gmail
// files the sent copy in Sent itself; other servers get a copy APPENDed to
// their Sent mailbox. The body is sent exactly as stored; only these
// headers change: a fresh Message-ID (Gmail drops a message whose
// Message-ID it already holds, and the draft still holds the old one until
// it is removed), Date set to the time of sending, and Bcc removed (Bcc
// recipients get the message through the SMTP envelope).
func (p *Provider) Send(ctx context.Context, id string) (*provider.SendResult, error) {
	if p.cfg.Mode != policy.ModeSend {
		return nil, errs.Policy("account is in %s mode; the IMAP provider connects to SMTP only in send mode", p.cfg.Mode)
	}
	if p.cfg.SMTPHost == "" {
		return nil, errs.Usage("imap account %s has no SMTP host; log in again with --smtp-host", p.cfg.Alias)
	}
	l, err := p.draftAt(ctx, id)
	if err != nil {
		return nil, err
	}
	fs, err := p.fetch(ctx, l.box, []uint32{l.uid}, true)
	if err != nil {
		return nil, err
	}
	if len(fs) == 0 {
		return nil, errs.Provider("notFound", fmt.Errorf("draft %s not found", id))
	}
	draft := fs[0]
	threadID := p.convert(ctx, draft).ThreadID

	now := time.Now()
	msg, env, err := prepareSubmission(draft.raw, p.cfg.Username, p.domain(), now)
	if err != nil {
		return nil, err
	}
	if err := p.submit(ctx, env, msg); err != nil {
		return nil, err
	}
	res := &provider.SendResult{InternetMessageID: env.messageID, ThreadID: threadID, SentAt: now.UTC().Truncate(time.Second)}

	// The message is out. What follows is cleanup: a failure is reported
	// on stderr but must not turn the send into an error (a retry would
	// send it twice).
	if err := p.remove(ctx, id, l); err != nil {
		fmt.Fprintf(os.Stderr, "warning: the message was sent but draft %s could not be removed: %v\n", id, err)
	}
	if !p.gmail {
		if sent, _ := p.byRole(ctx, provider.LabelSent); sent != "" {
			if _, err := p.appendTo(ctx, sent, `(\Seen)`, msg, env.messageID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: the message was sent but not saved to %s: %v\n", sent, err)
			}
		}
	}
	return res, nil
}

type envelope struct {
	from      string
	rcpt      []string
	messageID string
}

// prepareSubmission completes the headers of a stored draft for SMTP and
// returns the envelope. The draft's From must be the account's address.
func prepareSubmission(raw []byte, self, domain string, now time.Time) ([]byte, *envelope, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	tp := textproto.NewReader(br)
	h, err := tp.ReadMIMEHeader()
	if err != nil && !(errors.Is(err, io.EOF) && len(h) > 0) {
		return nil, nil, errs.Provider("badDraft", fmt.Errorf("the draft's header cannot be parsed: %v", err))
	}
	body, _ := io.ReadAll(br)

	from := mailmime.ParseAddr(h.Get("From"))
	if from.Email == "" {
		h.Set("From", self)
	} else if !strings.EqualFold(from.Email, self) {
		return nil, nil, errs.Usage("the draft's From is %s, not this account (%s); it cannot be sent from here", from.Email, self)
	}
	env := &envelope{from: self}
	seen := map[string]bool{}
	for _, k := range []string{"To", "Cc", "Bcc"} {
		for _, a := range mailmime.ParseAddrList(strings.Join(h.Values(k), ", ")) {
			addr := strings.TrimSpace(a.Email)
			if addr == "" || seen[strings.ToLower(addr)] {
				continue
			}
			if _, err := mail.ParseAddress(addr); err != nil || strings.ContainsAny(addr, "\r\n<>") {
				return nil, nil, errs.Usage("the draft has an invalid recipient %q", addr)
			}
			seen[strings.ToLower(addr)] = true
			env.rcpt = append(env.rcpt, addr)
		}
	}
	if len(env.rcpt) == 0 {
		return nil, nil, errs.Usage("the draft has no recipients")
	}
	env.messageID = mailmime.NewMessageID(domain)
	h.Del("Bcc")
	h.Del("Message-Id")
	h.Del("Date")

	// Re-serialise: our headers first, then the stored ones in their
	// original order (textproto lost the order, so take it from raw).
	var out bytes.Buffer
	fmt.Fprintf(&out, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&out, "Message-ID: %s\r\n", env.messageID)
	writeHeadersInOrder(&out, raw, h)
	out.WriteString("\r\n")
	out.Write(body)
	return out.Bytes(), env, nil
}

// writeHeadersInOrder writes the fields of h, in the order they appear in
// raw's header block, folded as they were.
func writeHeadersInOrder(w *bytes.Buffer, raw []byte, h textproto.MIMEHeader) {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if end < 0 {
		end = bytes.Index(raw, []byte("\n\n"))
	}
	if end < 0 {
		end = len(raw)
	}
	lines := strings.Split(strings.ReplaceAll(string(raw[:end]), "\r\n", "\n"), "\n")
	keep := false
	written := map[string]bool{}
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		if ln[0] == ' ' || ln[0] == '\t' {
			if keep {
				w.WriteString(ln + "\r\n")
			}
			continue
		}
		k, _, _ := strings.Cut(ln, ":")
		ck := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(k))
		_, keep = h[ck]
		if keep {
			w.WriteString(ln + "\r\n")
			written[ck] = true
		}
	}
	// Fields set here that the stored header did not have (From).
	for k, vs := range h {
		if !written[k] {
			for _, v := range vs {
				fmt.Fprintf(w, "%s: %s\r\n", k, mailmime.NoCRLF(v))
			}
		}
	}
}

// submit sends one message: STARTTLS (required) on 25 and 587, implicit
// TLS elsewhere (465), then AUTH PLAIN with the account's password.
func (p *Provider) submit(ctx context.Context, env *envelope, msg []byte) error {
	host, port := p.cfg.SMTPHost, p.cfg.SMTPPort
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	cfg := tlsConfig(p.cfg.TLS, host)
	d := &net.Dialer{Timeout: 30 * time.Second}
	var nc net.Conn
	var err error
	if smtpStartTLSPorts[port] {
		nc, err = d.DialContext(ctx, "tcp", addr)
	} else {
		nc, err = (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return smtpErr(ctx, "connect", err)
	}
	defer nc.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	sc, err := smtp.NewClient(nc, host)
	if err != nil {
		return smtpErr(ctx, "greeting", err)
	}
	defer sc.Close()
	if err := sc.Hello("localhost"); err != nil {
		return smtpErr(ctx, "EHLO", err)
	}
	if _, isTLS := nc.(*tls.Conn); !isTLS {
		if ok, _ := sc.Extension("STARTTLS"); !ok {
			return errs.Provider("noTLS", errors.New("the SMTP server offers no STARTTLS on this port; use port 465 (implicit TLS)"))
		}
		if err := sc.StartTLS(cfg); err != nil {
			return smtpErr(ctx, "STARTTLS", err)
		}
	}
	if err := sc.Auth(smtp.PlainAuth("", p.cfg.Username, p.cfg.Password, host)); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) && (te.Code == 535 || te.Code == 534) {
			return errs.Auth("SMTP login as %s failed (%s); check the app password", p.cfg.Username, te.Msg)
		}
		return smtpErr(ctx, "AUTH", err)
	}
	if err := sc.Mail(env.from); err != nil {
		return smtpErr(ctx, "MAIL FROM", err)
	}
	for _, r := range env.rcpt {
		if err := sc.Rcpt(r); err != nil {
			return smtpErr(ctx, "RCPT TO <"+r+">", err)
		}
	}
	w, err := sc.Data()
	if err != nil {
		return smtpErr(ctx, "DATA", err)
	}
	if _, err := w.Write(msg); err != nil {
		return smtpErr(ctx, "DATA", err)
	}
	if err := w.Close(); err != nil {
		return smtpErr(ctx, "DATA", err)
	}
	_ = sc.Quit()
	return nil
}

func smtpErr(ctx context.Context, step string, err error) error {
	if ctx.Err() != nil {
		return errs.Timeout(ctx.Err())
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errs.Timeout(err)
	}
	code := "smtp"
	var te *textproto.Error
	if errors.As(err, &te) {
		code = "smtp" + strconv.Itoa(te.Code)
	}
	return errs.Provider(code, fmt.Errorf("SMTP %s: %w", step, err))
}
