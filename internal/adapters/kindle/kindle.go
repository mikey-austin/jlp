// Package kindle delivers ebooks to a Kindle through Amazon's Send to
// Kindle e-mail service: the EPUB is mailed as an attachment from an
// address the learner has approved in their Amazon account ("Approved
// Personal Document E-mail List") to their device's @kindle.com address.
//
// Unlike internal/adapters/smtp — the deliberately minimal no-auth,
// no-TLS adapter for the LAN Mailpit — this one talks to a real
// submission server (Gmail, Fastmail, iCloud, SES …): STARTTLS or
// implicit TLS, and PLAIN authentication. It refuses to send a password
// over an unencrypted connection; net/smtp enforces that for any host
// other than localhost, and config validation rejects the combination
// up front.
package kindle

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

// TLS modes.
const (
	TLSStartTLS = "starttls" // port 587: plain connect, then STARTTLS (required)
	TLSImplicit = "tls"      // port 465: TLS from the first byte
	TLSNone     = "none"     // Mailpit / a local relay only
)

// maxAttachment is Send to Kindle's documented e-mail limit (50 MB).
// A study edition is tens of kilobytes; this is a sanity bound.
const maxAttachment = 50 << 20

// Config configures the sender.
type Config struct {
	Addr     string // host:port of the submission server
	From     string // must be on the Amazon account's approved list
	Username string // empty: no authentication
	Password string
	TLS      string // starttls (default) | tls | none
	Timeout  time.Duration
}

// Sender implements publishing.Deliverer.
type Sender struct {
	cfg Config
	// dial is swapped in tests.
	dial func(ctx context.Context) (*smtp.Client, error)
}

var _ publishing.Deliverer = (*Sender)(nil)

// New returns a Sender. Like the other adapters it never dials at
// construction: an unreachable mail server must not stop the app
// booting.
func New(cfg Config) *Sender {
	if cfg.TLS == "" {
		cfg.TLS = TLSStartTLS
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Minute
	}
	s := &Sender{cfg: cfg}
	s.dial = s.dialServer
	return s
}

func (s *Sender) host() string {
	h, _, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return s.cfg.Addr
	}
	return h
}

func (s *Sender) dialServer(ctx context.Context) (*smtp.Client, error) {
	d := net.Dialer{Timeout: s.cfg.Timeout}
	var conn net.Conn
	var err error
	if s.cfg.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: s.host(), MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", s.cfg.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.cfg.Addr)
	}
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(s.cfg.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.host())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if s.cfg.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			_ = c.Close()
			return nil, fmt.Errorf("%w: %s does not offer STARTTLS", publishing.ErrPermanent, s.cfg.Addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.host(), MinVersion: tls.VersionTLS12}); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// Deliver mails p.Data as an attachment to p.To.
func (s *Sender) Deliver(ctx context.Context, p publishing.Parcel) error {
	if len(p.Data) > maxAttachment {
		return fmt.Errorf("%w: attachment is %d bytes, over Send to Kindle's limit", publishing.ErrPermanent, len(p.Data))
	}
	msg, err := BuildMessage(s.cfg.From, p, time.Now())
	if err != nil {
		return fmt.Errorf("%w: %v", publishing.ErrPermanent, err)
	}
	c, err := s.dial(ctx)
	if err != nil {
		return classify("connect", err)
	}
	// Close after a successful Quit only reports "already closed".
	defer func() { _ = c.Close() }()
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.host())); err != nil {
			return classify("auth", err)
		}
	}
	if err := c.Mail(addrOnly(s.cfg.From)); err != nil {
		return classify("MAIL FROM", err)
	}
	if err := c.Rcpt(addrOnly(p.To)); err != nil {
		return classify("RCPT TO", err)
	}
	w, err := c.Data()
	if err != nil {
		return classify("DATA", err)
	}
	if _, err := w.Write(msg); err != nil {
		return classify("DATA", err)
	}
	if err := w.Close(); err != nil {
		return classify("DATA", err)
	}
	return classify("QUIT", c.Quit())
}

// classify marks an SMTP 5xx reply (the server's definitive "no": bad
// recipient, rejected sender, failed authentication) as permanent, so
// the pipeline stops instead of retrying into the same wall. 4xx replies
// and network errors stay retryable.
func classify(stage string, err error) error {
	if err == nil {
		return nil
	}
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return fmt.Errorf("%w: smtp %s: %v", publishing.ErrPermanent, stage, err)
	}
	return fmt.Errorf("smtp %s: %w", stage, err)
}

// addrOnly strips a display name: "JLP <me@example.com>" → the address.
func addrOnly(s string) string {
	s = strings.TrimSpace(s)
	if i, j := strings.LastIndex(s, "<"), strings.LastIndex(s, ">"); i >= 0 && j > i {
		return s[i+1 : j]
	}
	return s
}

// BuildMessage renders the RFC 5322 multipart message: a one-line text
// body and the ebook as a base64 attachment. The filename is sent twice
// — an ASCII fallback, and the real (often Japanese) name RFC 2231
// encoded — because mail systems differ in which one they read. Send to
// Kindle titles an EPUB from its own metadata, so the name is only what
// the learner sees in their Amazon library's document list.
func BuildMessage(from string, p publishing.Parcel, now time.Time) ([]byte, error) {
	for _, h := range []string{from, p.To} {
		if strings.ContainsAny(h, "\r\n") {
			return nil, errors.New("header value contains a line break")
		}
	}
	mediaType := p.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	boundary := "jlp-" + fmt.Sprintf("%x", now.UnixNano())
	subject := strings.NewReplacer("\r", " ", "\n", " ").Replace(p.Subject)
	if subject == "" {
		subject = "JLP"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", p.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString("JLP 読解 — Kindle edition attached.\r\n\r\n")

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	fmt.Fprintf(&b, "Content-Type: %s\r\n", mediaType)
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	fmt.Fprintf(&b, "Content-Disposition: attachment; filename=%q; filename*=UTF-8''%s\r\n\r\n",
		asciiFilename(p.Filename), rfc2231(p.Filename))
	enc := base64.StdEncoding.EncodeToString(p.Data)
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String()), nil
}

// asciiFilename keeps the ASCII parts of name (the date, the extension)
// and drops the rest: "JLP 2026-09-29 経済対策.epub" → "JLP_2026-09-29.epub".
func asciiFilename(name string) string {
	ext := ""
	if i := strings.LastIndex(name, "."); i > 0 {
		name, ext = name[:i], name[i:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if out == "" {
		out = "jlp"
	}
	return out + ext
}

// rfc2231 percent-encodes s for a filename* parameter.
func rfc2231(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var b strings.Builder
	for _, c := range []byte(s) {
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
