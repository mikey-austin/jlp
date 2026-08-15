package smtp_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/smtp"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/notifications"
)

// fakeSMTPServer is a minimal SMTP server good enough to exercise
// net/smtp's client: it accepts exactly one connection, answers
// EHLO/MAIL FROM/RCPT TO/DATA/QUIT with plain 2xx/3xx replies (no
// extensions advertised, so the client never attempts STARTTLS or
// AUTH), and records every command line plus every DATA line for the
// test to assert on — the brief's "fake SMTP listener asserting MAIL
// FROM/RCPT TO/DATA contains subject+body" scenario.
type fakeSMTPServer struct {
	ln   net.Listener
	done chan struct{}

	mu        sync.Mutex
	commands  []string
	dataLines []string
}

func startFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTPServer{ln: ln, done: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serveOne()
	return s
}

func (s *fakeSMTPServer) addr() string { return s.ln.Addr().String() }

// waitDone blocks until serveOne has handled QUIT (or the connection
// closed), so assertions run only after every command/DATA line has
// actually been recorded — otherwise they'd race the goroutine.
func (s *fakeSMTPServer) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake SMTP server never saw QUIT")
	}
}

func (s *fakeSMTPServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, line)
}

func (s *fakeSMTPServer) recordData(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dataLines = append(s.dataLines, line)
}

func (s *fakeSMTPServer) serveOne() {
	defer close(s.done)
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	r := bufio.NewReader(conn)
	writeLine := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	writeLine("220 fake.smtp ESMTP")
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				inData = false
				writeLine("250 OK: queued")
				continue
			}
			// RFC 5321 dot-stuffing: a body line that itself starts with
			// "." arrives with one extra leading "." prepended by the
			// client; undo that here so recorded DATA lines match what
			// the caller actually asked to send.
			s.recordData(strings.TrimPrefix(line, "."))
			continue
		}

		s.record(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			writeLine("250 fake.smtp")
		case strings.HasPrefix(upper, "MAIL FROM"):
			// Mailpit (and any RFC 5321-conformant server) rejects a MAIL
			// FROM envelope carrying a display-name-wrapped mailbox
			// ("MAIL FROM:<Name <addr>>") with a syntax error — this is
			// what caught the real bug in Send's first draft (it passed a
			// "Name <addr>" string straight into net/smtp.SendMail's bare
			// `from` argument), which manual verification against a real
			// Mailpit surfaced but this fake server's original
			// unconditional "250 OK" did not. Enforcing the same rule here
			// closes that gap for future regressions.
			if !validMailFromSyntax(line) {
				writeLine("501 5.5.4 Syntax error in parameters or arguments (invalid FROM parameter)")
				continue
			}
			writeLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			writeLine("250 OK")
		case upper == "DATA":
			inData = true
			writeLine("354 Start mail input; end with <CRLF>.<CRLF>")
		case upper == "QUIT":
			writeLine("221 Bye")
			return
		default:
			writeLine("500 unrecognized command")
		}
	}
}

// validMailFromSyntax reports whether line's "MAIL FROM:<...>" argument
// is a bare address — no embedded whitespace or nested angle brackets —
// the same narrow check a real SMTP server applies to the envelope
// sender (see the MAIL FROM case above for why this exists).
func validMailFromSyntax(line string) bool {
	idx := strings.Index(line, ":")
	if idx == -1 {
		return false
	}
	rest := strings.TrimSpace(line[idx+1:])
	if !strings.HasPrefix(rest, "<") {
		return false
	}
	end := strings.Index(rest, ">")
	if end == -1 {
		return false
	}
	addr := rest[1:end]
	return addr != "" && !strings.ContainsAny(addr, " \t<>")
}

func containsPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.ToUpper(l), strings.ToUpper(prefix)) {
			return true
		}
	}
	return false
}

// TestSendDeliversMailFromRcptToAndDataWithSubjectAndBody pins the
// brief's Step 1 scenario: Send drives a real (if minimal) SMTP
// transcript — MAIL FROM the fixed envelope sender, RCPT TO the
// notification's recipient, and a DATA payload whose headers/body carry
// the subject and text body verbatim (including Japanese content, sent
// unencoded per buildMessage's Content-Transfer-Encoding: 8bit).
func TestSendDeliversMailFromRcptToAndDataWithSubjectAndBody(t *testing.T) {
	srv := startFakeSMTPServer(t)
	notifier := smtp.New(config.SMTP{Addr: srv.addr()})

	err := notifier.Send(context.Background(), notifications.Notification{
		To:       "learner@jlp.local",
		Subject:  "今週の学習まとめ",
		TextBody: "What you accomplished this week:\n- 面白かったです\n",
	})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	srv.waitDone(t)

	// Pins the envelope-vs-header From split (see smtp.go's envelopeAddr/
	// headerFrom doc comment): MAIL FROM must be the BARE address, not
	// the display-name form used in the "From:" header below — a real
	// SMTP server (Mailpit included) rejects "MAIL FROM:<Name <addr>>"
	// outright, which is exactly what this fake server's validMailFrom
	// Syntax check above now also enforces.
	if !containsPrefix(srv.commands, "MAIL FROM:<jlp@localhost>") {
		t.Fatalf("commands = %v, want MAIL FROM:<jlp@localhost> (bare address)", srv.commands)
	}
	if !containsPrefix(srv.commands, "RCPT TO:<learner@jlp.local>") {
		t.Fatalf("commands = %v, want RCPT TO:<learner@jlp.local>", srv.commands)
	}

	data := strings.Join(srv.dataLines, "\n")
	if !strings.Contains(data, "From: JLP <jlp@localhost>") {
		t.Fatalf("DATA missing From header: %s", data)
	}
	if !strings.Contains(data, "To: learner@jlp.local") {
		t.Fatalf("DATA missing To header: %s", data)
	}
	if !strings.Contains(data, "Subject:") {
		t.Fatalf("DATA missing Subject header: %s", data)
	}
	if !strings.Contains(data, "Content-Type: text/plain") {
		t.Fatalf("DATA missing plain-text Content-Type: %s", data)
	}
	if !strings.Contains(data, "What you accomplished this week:") {
		t.Fatalf("DATA missing body heading: %s", data)
	}
	if !strings.Contains(data, "面白かったです") {
		t.Fatalf("DATA missing body content (面白かったです): %s", data)
	}
}

// TestSendReturnsErrorWhenServerUnreachable pins the "real delivery
// failure propagates" contract (notifications.Notifier's own doc
// comment): dialing a closed port must surface as an error, not be
// swallowed.
func TestSendReturnsErrorWhenServerUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing is listening on addr anymore

	notifier := smtp.New(config.SMTP{Addr: addr})
	err = notifier.Send(context.Background(), notifications.Notification{
		To:       "learner@jlp.local",
		Subject:  "subject",
		TextBody: "body",
	})
	if err == nil {
		t.Fatal("Send returned nil error, want a connection failure")
	}
}
