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
	mu        sync.Mutex
	data      string
	rcpts     []string
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
			if s.starttls {
				w("250-fake")
				w("250 STARTTLS")
			} else {
				w("250 fake")
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
