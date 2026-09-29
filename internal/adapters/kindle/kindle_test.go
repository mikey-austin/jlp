package kindle

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

// fakeServer is a minimal SMTP server: enough of RFC 5321 for net/smtp's
// client, with scriptable replies to RCPT.
type fakeServer struct {
	ln        net.Listener
	rcptReply string
	starttls  bool
	auth      string // AUTH mechanisms to advertise; empty advertises none
	mu        sync.Mutex
	data      string
	rcpts     []string
	mech      string // the mechanism the client authenticated with
	creds     string // "user:pass" as the server received them
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, rcptReply: "250 ok"}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *fakeServer) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	w := func(line string) { _, _ = io.WriteString(c, line+"\r\n") }
	w("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			exts := []string{"fake"}
			if s.starttls {
				exts = append(exts, "STARTTLS")
			}
			if s.auth != "" {
				exts = append(exts, "AUTH "+s.auth)
			}
			for i, e := range exts {
				if i == len(exts)-1 {
					w("250 " + e)
				} else {
					w("250-" + e)
				}
			}
		case strings.HasPrefix(cmd, "AUTH "):
			if !s.authenticate(r, w, strings.Fields(strings.TrimSpace(line))[1:]) {
				return
			}
		case strings.HasPrefix(cmd, "MAIL FROM"):
			w("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			s.mu.Lock()
			s.rcpts = append(s.rcpts, strings.TrimSpace(line))
			s.mu.Unlock()
			w(s.rcptReply)
		case cmd == "DATA":
			w("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			w("250 queued")
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

// authenticate plays the server side of AUTH PLAIN and AUTH LOGIN,
// refusing any mechanism it did not advertise the way a real server
// does (KPN answers PLAIN with 504 5.5.4).
func (s *fakeServer) authenticate(r *bufio.Reader, w func(string), args []string) bool {
	mech := strings.ToUpper(args[0])
	if !strings.Contains(" "+strings.ToUpper(s.auth)+" ", " "+mech+" ") {
		w(`504 5.5.4 Unsupported AUTH mechanism`)
		return true
	}
	read := func(prompt string) (string, bool) {
		w("334 " + base64.StdEncoding.EncodeToString([]byte(prompt)))
		l, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
		return string(b), true
	}
	var creds string
	switch mech {
	case "PLAIN":
		resp := ""
		if len(args) > 1 {
			b, _ := base64.StdEncoding.DecodeString(args[1])
			resp = string(b)
		} else {
			var ok bool
			if resp, ok = read(""); !ok {
				return false
			}
		}
		parts := strings.SplitN(resp, "\x00", 3)
		if len(parts) == 3 {
			creds = parts[1] + ":" + parts[2]
		}
	case "LOGIN":
		user, ok := read("Username:")
		if !ok {
			return false
		}
		pass, ok := read("Password:")
		if !ok {
			return false
		}
		creds = user + ":" + pass
	}
	s.mu.Lock()
	s.mech, s.creds = mech, creds
	s.mu.Unlock()
	w("235 2.7.0 authenticated")
	return true
}

func parcel() publishing.Parcel {
	return publishing.Parcel{
		To:        "me@kindle.com",
		Subject:   "政府、新たな経済対策",
		Filename:  "JLP 2026-09-29 経済対策.epub",
		MediaType: "application/epub+zip",
		Data:      bytes.Repeat([]byte("PK\x03\x04epub"), 50),
	}
}

func sender(s *fakeServer) *Sender {
	return New(Config{Addr: s.ln.Addr().String(), From: "JLP <me@example.com>", TLS: TLSNone, Timeout: 5 * time.Second})
}

func TestDeliverSendsAttachment(t *testing.T) {
	srv := newFakeServer(t)
	p := parcel()
	if err := sender(srv).Deliver(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	raw, rcpts := srv.data, srv.rcpts
	srv.mu.Unlock()
	if len(rcpts) != 1 || !strings.Contains(rcpts[0], "<me@kindle.com>") {
		t.Fatalf("rcpts = %v", rcpts)
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	subj, _ := dec.DecodeHeader(msg.Header.Get("Subject"))
	if subj != p.Subject {
		t.Fatalf("subject = %q", subj)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content-type = %q %v", mt, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var found bool
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Type") != "application/epub+zip" {
			continue
		}
		found = true
		// Go's multipart decodes filename* (RFC 2231) for us.
		if part.FileName() != p.Filename {
			t.Fatalf("filename = %q", part.FileName())
		}
		if !strings.Contains(part.Header.Get("Content-Disposition"), `filename="JLP_2026-09-29.epub"`) {
			t.Fatalf("ascii fallback missing: %s", part.Header.Get("Content-Disposition"))
		}
		body, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if !bytes.Equal(body, p.Data) {
			t.Fatal("attachment did not round-trip")
		}
	}
	if !found {
		t.Fatalf("no attachment in:\n%s", raw)
	}
}

func TestDeliverClassifiesFailures(t *testing.T) {
	srv := newFakeServer(t)
	srv.rcptReply = "550 5.1.1 mailbox unavailable"
	err := sender(srv).Deliver(context.Background(), parcel())
	if !errors.Is(err, publishing.ErrPermanent) {
		t.Fatalf("5xx should be permanent: %v", err)
	}
	srv.rcptReply = "451 4.3.0 try again later"
	err = sender(srv).Deliver(context.Background(), parcel())
	if err == nil || errors.Is(err, publishing.ErrPermanent) {
		t.Fatalf("4xx should be retryable: %v", err)
	}
}

func TestDeliverRequiresSTARTTLSWhenConfigured(t *testing.T) {
	srv := newFakeServer(t) // advertises no STARTTLS
	s := New(Config{Addr: srv.ln.Addr().String(), From: "me@example.com", TLS: TLSStartTLS, Timeout: 5 * time.Second})
	if err := s.Deliver(context.Background(), parcel()); !errors.Is(err, publishing.ErrPermanent) {
		t.Fatalf("plaintext fallback must be refused: %v", err)
	}
}

func TestDeliverUnreachableIsRetryable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	s := New(Config{Addr: addr, From: "me@example.com", TLS: TLSNone, Timeout: time.Second})
	err := s.Deliver(context.Background(), parcel())
	if err == nil || errors.Is(err, publishing.ErrPermanent) {
		t.Fatalf("connection refused should be retryable: %v", err)
	}
}

func TestBuildMessageRejectsHeaderInjection(t *testing.T) {
	p := parcel()
	p.To = "me@kindle.com\r\nBcc: attacker@example.com"
	if _, err := BuildMessage("me@example.com", p, time.Now()); err == nil {
		t.Fatal("CRLF in a recipient must be rejected")
	}
}

func TestAddrOnly(t *testing.T) {
	for in, want := range map[string]string{"JLP <a@b.c>": "a@b.c", " a@b.c ": "a@b.c"} {
		if got := addrOnly(in); got != want {
			t.Fatalf("addrOnly(%q) = %q", in, got)
		}
	}
}

func TestDeliverAuthenticatesWithWhatTheServerOffers(t *testing.T) {
	for _, tc := range []struct{ offered, want string }{
		{"LOGIN", "LOGIN"},       // KPN: LOGIN only, PLAIN is a 504
		{"PLAIN LOGIN", "PLAIN"}, // PLAIN stays the preference
		{"LOGIN PLAIN", "PLAIN"}, // whatever the advertised order
		{"CRAM-MD5 PLAIN", "PLAIN"},
	} {
		srv := newFakeServer(t)
		srv.auth = tc.offered
		s := New(Config{Addr: srv.ln.Addr().String(), From: "me@example.com", Username: "mikey@kpnmail.nl", Password: "s3cret", TLS: TLSNone, Timeout: 5 * time.Second})
		if err := s.Deliver(context.Background(), parcel()); err != nil {
			t.Fatalf("offered %q: %v", tc.offered, err)
		}
		srv.mu.Lock()
		mech, creds := srv.mech, srv.creds
		srv.mu.Unlock()
		if mech != tc.want || creds != "mikey@kpnmail.nl:s3cret" {
			t.Fatalf("offered %q: authenticated with %q as %q", tc.offered, mech, creds)
		}
	}
}

func TestDeliverWithNoUsableMechanismIsPermanent(t *testing.T) {
	srv := newFakeServer(t)
	srv.auth = "CRAM-MD5"
	s := New(Config{Addr: srv.ln.Addr().String(), From: "me@example.com", Username: "u", Password: "p", TLS: TLSNone, Timeout: 5 * time.Second})
	if err := s.Deliver(context.Background(), parcel()); !errors.Is(err, publishing.ErrPermanent) {
		t.Fatalf("no PLAIN or LOGIN should be permanent: %v", err)
	}
}

func TestLoginAuthRefusesAnUnencryptedRemoteServer(t *testing.T) {
	a := loginAuth{username: "u", password: "p", host: "smtp.example.com"}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "smtp.example.com", TLS: false, Auth: []string{"LOGIN"}}); err == nil {
		t.Fatal("LOGIN must not send a password over plaintext to a remote host")
	}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "smtp.example.com", TLS: true, Auth: []string{"LOGIN"}}); err != nil {
		t.Fatalf("over TLS: %v", err)
	}
}
