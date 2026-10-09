package imap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// testTLS returns a server config with a fresh self-signed certificate for
// 127.0.0.1 and a client config that trusts only it.
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	client = &tls.Config{RootCAs: pool}
	return server, client
}

func hostPort(t *testing.T, addr net.Addr) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// ---- SMTP sink (go-smtp) ----

type sentMail struct {
	From string
	Rcpt []string
	Data []byte
}

type smtpSink struct {
	mu    sync.Mutex
	user  string
	pass  string
	conns int
	mails []sentMail
}

func (s *smtpSink) NewSession(*smtp.Conn) (smtp.Session, error) {
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	return &smtpSession{sink: s}, nil
}

func (s *smtpSink) sent() []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentMail(nil), s.mails...)
}

func (s *smtpSink) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

type smtpSession struct {
	sink   *smtpSink
	authed bool
	cur    sentMail
}

func (s *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *smtpSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != s.sink.user || pass != s.sink.pass {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "bad credentials"}
		}
		s.authed = true
		return nil
	}), nil
}

func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authed {
		return &smtp.SMTPError{Code: 530, Message: "auth required"}
	}
	s.cur = sentMail{From: from}
	return nil
}

func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.cur.Rcpt = append(s.cur.Rcpt, to)
	return nil
}

func (s *smtpSession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.cur.Data = b
	s.sink.mu.Lock()
	s.sink.mails = append(s.sink.mails, s.cur)
	s.sink.mu.Unlock()
	return nil
}

func (s *smtpSession) Reset()        {}
func (s *smtpSession) Logout() error { return nil }

// startSMTP serves implicit TLS on a random port; with startTLS the port
// is registered as a STARTTLS port and the listener is plain.
func startSMTP(t *testing.T, serverTLS *tls.Config, user, pass string, startTLS bool) (*smtpSink, string, int) {
	t.Helper()
	sink := &smtpSink{user: user, pass: pass}
	srv := smtp.NewServer(sink)
	srv.Domain = "localhost"
	srv.TLSConfig = serverTLS
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := hostPort(t, ln.Addr())
	if startTLS {
		smtpStartTLSPorts[port] = true
		t.Cleanup(func() { delete(smtpStartTLSPorts, port) })
	} else {
		ln = tls.NewListener(ln, serverTLS)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sink, host, port
}
