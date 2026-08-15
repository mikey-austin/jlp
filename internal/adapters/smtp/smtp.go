// Package smtp implements notifications.Notifier over plain SMTP
// (net/smtp, standard library only): the deliberately simplest possible
// outbound-email adapter, sized for a LAN/dev Mailpit target (Phase 3
// Task 5, PRD §21, §65) rather than a public relay — no authentication,
// no TLS/STARTTLS negotiation. A production deployment that needs
// either is a later adapter (or a later extension of this one), not
// something this task's scope covers.
package smtp

import (
	"context"
	"fmt"
	"mime"
	"net/smtp"
	"strings"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/notifications"
)

// envelopeAddr is the bare address net/smtp.SendMail's `from` argument
// requires — it becomes the SMTP envelope's `MAIL FROM:<...>` command,
// which (per RFC 5321) takes a plain address, NOT a display-name-
// wrapped one; a real SMTP server (Mailpit included) rejects
// "Name <addr>" there with a syntax error. headerFrom is the
// display-name form used in the message's own "From:" header, where
// RFC 5322's mailbox syntax allows it.
//
// Both are fixed. notifications.Notification — the port every Notifier
// implements — deliberately carries no From field (a future non-email
// channel, e.g. Slack, has no use for an email-shaped sender address;
// see that package's doc comment), so this adapter supplies its own
// rather than accept one in Notification. config.Summary.From is
// reserved for wiring a configurable sender into a future version of
// this adapter — see that field's own doc comment for why it isn't
// consumed here.
const (
	envelopeAddr = "jlp@localhost"
	headerFrom   = "JLP <jlp@localhost>"
)

// client is the notifications.Notifier this package satisfies.
type client struct {
	addr string
}

// New returns an SMTP-backed Notifier talking to cfg.Addr (host:port,
// e.g. "mailpit:1025", the docker-compose "mail" profile's in-network
// address — see config.SMTP's doc comment). Construction never fails:
// no connection is opened until Send is actually called, matching
// internal/adapters/clicmd's "never errors at construction" contract
// for the analogous reason (a dormant, never-called Notifier must not
// block boot on network reachability it doesn't need yet).
func New(cfg config.SMTP) notifications.Notifier {
	return &client{addr: cfg.Addr}
}

// Send delivers n over SMTP as one plain-text message: MAIL FROM
// envelopeAddr, RCPT TO n.To, DATA carrying a minimal RFC 5322 message
// (buildMessage). No authentication (nil smtp.Auth) — appropriate only
// for a Mailpit/LAN target, per the package doc comment. A delivery
// failure is returned as a real error (see notifications.Notifier's own
// doc comment: there is nothing durable to fall back on here).
func (c *client) Send(_ context.Context, n notifications.Notification) error {
	if err := smtp.SendMail(c.addr, nil, envelopeAddr, []string{n.To}, buildMessage(n)); err != nil {
		return fmt.Errorf("smtp: send to %s via %s: %w", n.To, c.addr, err)
	}
	return nil
}

// buildMessage renders n as a minimal RFC 5322 message: headers, a
// blank line, then n.TextBody verbatim (converted to CRLF line endings,
// which SMTP's DATA command requires). Subject is MIME-encoded
// (RFC 2047, UTF-8 Q-encoding) since it routinely contains Japanese
// text — see internal/agent/summary's weekly_summary.v1 subject, which
// always does in practice; the body is sent as raw UTF-8 octets
// (Content-Transfer-Encoding: 8bit) rather than quoted-printable —
// Mailpit and any other reasonably modern SMTP receiver handle this
// without issue, and it keeps the message trivially readable as plain
// text, unencoded, straight in the DATA payload.
//
// n.To is stripped of any embedded CR/LF before being written into a
// header: header injection via a crafted recipient address is remote
// (To always comes from trusted config or an operator-supplied CLI
// flag in this codebase — see cmd/jlp/summary.go), but stripping costs
// nothing and closes the theoretical gap outright.
func buildMessage(n notifications.Notification) []byte {
	to := stripCRLF(n.To)
	subject := mime.QEncoding.Encode("UTF-8", n.Subject)

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", headerFrom)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(n.TextBody, "\n", "\r\n"))
	return []byte(b.String())
}

func stripCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", "")
}
