package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/channels"
)

// capturingHandler is a minimal slog.Handler that records every log
// message, mirroring internal/adapters/slack/slack_test.go's own
// helper of the same name and purpose: let a test assert something WAS
// logged (a panic, a dropped malformed line) without depending on
// stdout/stderr capture.
type capturingHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) hasMessageContaining(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.messages {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// fakeTransport is transport's test double — no network, no real
// signal-cli connection. Run blocks (like the real transport.Run does)
// until ctx is done, and every SendMessage call is captured for
// assertions. Mirrors internal/adapters/slack's own fakeTransport.
type fakeTransport struct {
	events chan Event

	mu    sync.Mutex
	sends []sendCall

	sendErr error
}

type sendCall struct{ recipient, text string }

func newFakeTransport() *fakeTransport {
	return &fakeTransport{events: make(chan Event, 8)}
}

func (f *fakeTransport) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (f *fakeTransport) Events() <-chan Event { return f.events }

func (f *fakeTransport) SendMessage(_ context.Context, recipient, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sendCall{recipient, text})
	return f.sendErr
}

func (f *fakeTransport) Sends() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.sends...)
}

// waitFor polls cond every 5ms for up to 2s, mirroring
// internal/adapters/slack/slack_test.go's own helper.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was never met within the timeout")
}

func TestAdapterName(t *testing.T) {
	a := newAdapter(newFakeTransport())
	if a.Name() != "signal" {
		t.Fatalf("Name() = %q, want \"signal\"", a.Name())
	}
}

// TestStartDispatchesEventToHandleAndSendsReply is the core event ->
// Inbound -> handle -> Outbound -> Send round trip, entirely over the
// fake transport.
func TestStartDispatchesEventToHandleAndSendsReply(t *testing.T) {
	ft := newFakeTransport()
	a := newAdapter(ft)

	var mu sync.Mutex
	var gotIn channels.Inbound
	handle := func(_ context.Context, in channels.Inbound) (channels.Outbound, error) {
		mu.Lock()
		gotIn = in
		mu.Unlock()
		return channels.Outbound{ThreadID: in.ThreadID, Text: "reply text"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, handle) }()

	ft.events <- Event{Source: "+15555550100", Text: "とても面白いでした"}

	waitFor(t, func() bool { return len(ft.Sends()) == 1 })
	sends := ft.Sends()
	if sends[0].recipient != "+15555550100" || sends[0].text != "reply text" {
		t.Fatalf("SendMessage call = %+v, want {+15555550100, \"reply text\"}", sends[0])
	}

	mu.Lock()
	in := gotIn
	mu.Unlock()
	if in.Channel != "signal" || in.ExternalID != "+15555550100" || in.Text != "とても面白いでした" || in.ThreadID != "+15555550100" {
		t.Fatalf("handle received Inbound = %+v, want Channel=signal ExternalID/ThreadID=+15555550100", in)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v after ctx cancellation, want nil", err)
	}
}

// TestStartSkipsBlankText: an event with empty text must never reach
// handle, and never produce a reply.
func TestStartSkipsBlankText(t *testing.T) {
	ft := newFakeTransport()
	a := newAdapter(ft)
	var handleCalled atomic.Bool
	handle := func(_ context.Context, in channels.Inbound) (channels.Outbound, error) {
		handleCalled.Store(true)
		return channels.Outbound{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Start(ctx, handle) }()

	ft.events <- Event{Source: "+15555550100", Text: "   "}

	time.Sleep(20 * time.Millisecond) // give a wrongly-dispatched handle call a chance to happen
	if handleCalled.Load() {
		t.Fatal("handle was called for a blank-text event")
	}
	if len(ft.Sends()) != 0 {
		t.Fatalf("SendMessage calls = %d, want 0 for a blank-text event", len(ft.Sends()))
	}
}

// TestStartLogsHandleErrorAndKeepsDispatching pins "one bad message
// must never end dispatch's loop": a handle error for one event must
// not stop dispatch from processing the next one, and must not make
// Start return.
func TestStartLogsHandleErrorAndKeepsDispatching(t *testing.T) {
	ft := newFakeTransport()
	a := newAdapter(ft)
	handle := func(_ context.Context, in channels.Inbound) (channels.Outbound, error) {
		if in.Text == "boom" {
			return channels.Outbound{}, errors.New("boom: handle failed")
		}
		return channels.Outbound{ThreadID: in.ThreadID, Text: "ok"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, handle) }()

	ft.events <- Event{Source: "+1", Text: "boom"}
	ft.events <- Event{Source: "+1", Text: "fine"}

	waitFor(t, func() bool { return len(ft.Sends()) == 1 })
	if got := ft.Sends()[0].text; got != "ok" {
		t.Fatalf("reply text = %q, want \"ok\" (the second event, after the first errored)", got)
	}

	select {
	case err := <-done:
		t.Fatalf("Start returned early (%v) after a handle error, want it to keep running", err)
	default:
	}
	cancel()
	<-done
}

// TestStartRecoversPanicInHandleAndKeepsDispatching pins "a channel
// failure must never take the app down" all the way through a PANIC,
// not just an error return — mirrors
// internal/adapters/slack/slack_test.go's own test of the identical
// contract; see that test's doc comment for the full "why".
func TestStartRecoversPanicInHandleAndKeepsDispatching(t *testing.T) {
	handler := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prev)

	ft := newFakeTransport()
	a := newAdapter(ft)
	handle := func(_ context.Context, in channels.Inbound) (channels.Outbound, error) {
		if in.Text == "panic" {
			panic("boom: handler exploded")
		}
		return channels.Outbound{ThreadID: in.ThreadID, Text: "ok"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx, handle) }()

	ft.events <- Event{Source: "+1", Text: "panic"}
	ft.events <- Event{Source: "+1", Text: "fine"}

	waitFor(t, func() bool { return len(ft.Sends()) == 1 })
	if got := ft.Sends()[0].text; got != "ok" {
		t.Fatalf("reply text = %q, want \"ok\" (the second event, after the first panicked)", got)
	}
	waitFor(t, func() bool { return handler.hasMessageContaining("panic") })

	select {
	case err := <-done:
		t.Fatalf("Start returned early (%v) after a handle panic, want it to keep running", err)
	default:
	}
	cancel()
	<-done
}

// TestSendRejectsEmptyThreadID pins Send's defensive check: a caller
// that (incorrectly) sends an Outbound with no ThreadID gets an error,
// not a SendMessage call with an empty recipient.
func TestSendRejectsEmptyThreadID(t *testing.T) {
	ft := newFakeTransport()
	a := newAdapter(ft)
	if err := a.Send(context.Background(), channels.Outbound{ThreadID: "", Text: "hi"}); err == nil {
		t.Fatal("expected an error for an empty ThreadID")
	}
	if len(ft.Sends()) != 0 {
		t.Fatalf("SendMessage calls = %d, want 0", len(ft.Sends()))
	}
}

// TestSendPropagatesTransportError pins that a transport-level failure
// (e.g. the daemon reporting "not registered") surfaces back to Send's
// caller rather than being silently swallowed.
func TestSendPropagatesTransportError(t *testing.T) {
	ft := newFakeTransport()
	ft.sendErr = errors.New("signal: send: [401] User +1 is not registered")
	a := newAdapter(ft)
	err := a.Send(context.Background(), channels.Outbound{ThreadID: "+1", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("Send() error = %v, want it to carry the transport's not-registered error", err)
	}
}

// --- jsonrpcTransport wire-level tests: handleLine/translateEnvelope
// exercised directly against hand-built JSON-RPC lines (no network at
// all) ---

func TestTranslateEnvelopeForwardsDataMessage(t *testing.T) {
	params := json.RawMessage(`{"envelope":{"source":"+15555550100","sourceNumber":"+15555550100","sourceDevice":1,"timestamp":1,"dataMessage":{"timestamp":1,"message":"こんにちは"}},"account":"+15555550199"}`)
	ev, ok := translateEnvelope(params)
	if !ok {
		t.Fatal("expected translateEnvelope to accept a well-formed dataMessage envelope")
	}
	if ev.Source != "+15555550100" || ev.Text != "こんにちは" {
		t.Fatalf("translateEnvelope = %+v, want Source=+15555550100 Text=こんにちは", ev)
	}
}

// TestTranslateEnvelopeFallsBackToSource pins that an envelope carrying
// only "source" (no "sourceNumber") still resolves — some signal-cli
// versions/message types omit sourceNumber.
func TestTranslateEnvelopeFallsBackToSource(t *testing.T) {
	params := json.RawMessage(`{"envelope":{"source":"+15555550100","dataMessage":{"message":"hi"}}}`)
	ev, ok := translateEnvelope(params)
	if !ok || ev.Source != "+15555550100" {
		t.Fatalf("translateEnvelope = %+v, ok=%v, want Source=+15555550100 ok=true", ev, ok)
	}
}

// TestTranslateEnvelopeIgnoresNonDataMessageNotifications pins that a
// receipt/typing/sync notification (no dataMessage at all — e.g. a
// delivery receipt for this bot's own outgoing message) is dropped,
// not forwarded as an empty-text Event.
func TestTranslateEnvelopeIgnoresNonDataMessageNotifications(t *testing.T) {
	params := json.RawMessage(`{"envelope":{"source":"+15555550100","receiptMessage":{"when":1}}}`)
	if _, ok := translateEnvelope(params); ok {
		t.Fatal("expected a receipt-only envelope (no dataMessage) to be dropped")
	}
}

func TestTranslateEnvelopeRejectsMalformedJSON(t *testing.T) {
	if _, ok := translateEnvelope(json.RawMessage(`not json`)); ok {
		t.Fatal("expected malformed params to be dropped")
	}
}

// TestHandleLineForwardsReceiveNotification pins handleLine's routing
// of a full "receive" notification line onto t.events.
func TestHandleLineForwardsReceiveNotification(t *testing.T) {
	tr := newJSONRPCTransport("unused:0", "+15555550199")
	line := []byte(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceNumber":"+15555550100","dataMessage":{"message":"練習"}}}}`)
	tr.handleLine(line)

	select {
	case ev := <-tr.events:
		if ev.Source != "+15555550100" || ev.Text != "練習" {
			t.Fatalf("forwarded Event = %+v, want Source=+15555550100 Text=練習", ev)
		}
	default:
		t.Fatal("expected a receive notification to be forwarded, nothing was")
	}
}

// TestHandleLineDropsMalformedJSONWithLog pins the brief's "malformed
// payload dropped with a log" requirement.
func TestHandleLineDropsMalformedJSONWithLog(t *testing.T) {
	handler := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prev)

	tr := newJSONRPCTransport("unused:0", "+15555550199")
	tr.handleLine([]byte(`{not valid json`))

	select {
	case ev := <-tr.events:
		t.Fatalf("expected nothing forwarded for malformed JSON, got %+v", ev)
	default:
	}
	if !handler.hasMessageContaining("malformed") {
		t.Fatalf("expected a log message about the malformed line, got: %v", handler.messages)
	}
}

// TestHandleLineDropsReceiveWithUnexpectedEnvelopeShapeWithLog pins the
// same "dropped with a log" contract for a syntactically valid
// JSON-RPC "receive" notification whose envelope translateEnvelope
// still rejects (no dataMessage).
func TestHandleLineDropsReceiveWithUnexpectedEnvelopeShapeWithLog(t *testing.T) {
	handler := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prev)

	tr := newJSONRPCTransport("unused:0", "+15555550199")
	tr.handleLine([]byte(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+1","receiptMessage":{}}}}`))

	select {
	case ev := <-tr.events:
		t.Fatalf("expected nothing forwarded for a receipt-only envelope, got %+v", ev)
	default:
	}
	if !handler.hasMessageContaining("envelope") {
		t.Fatalf("expected a log message about the envelope shape, got: %v", handler.messages)
	}
}

// TestHandleLineDoesNotBlockOnDuplicateResponseForSameID pins a fix: a
// daemon that (buggily) sends TWO response lines for the same request
// id must never block handleLine — and therefore the single goroutine
// that reads every line off the connection, both responses and every
// future inbound message — on a full, already-satisfied respCh nobody
// will ever drain a second time. Regression test for the fix; run with
// a 2s deadline of its own so a reintroduced blocking send fails this
// test instead of hanging the whole suite.
func TestHandleLineDoesNotBlockOnDuplicateResponseForSameID(t *testing.T) {
	tr := newJSONRPCTransport("unused:0", "+15555550199")
	respCh := make(chan rpcResponse, 1)
	tr.pendingMu.Lock()
	tr.pending[9] = respCh
	tr.pendingMu.Unlock()

	handlerDone := make(chan struct{})
	go func() {
		tr.handleLine([]byte(`{"jsonrpc":"2.0","result":{"timestamp":1},"id":9}`))
		// Second line for the SAME id, before anyone has drained respCh
		// — with the old blocking `ch <- resp` this would hang forever.
		tr.handleLine([]byte(`{"jsonrpc":"2.0","result":{"timestamp":2},"id":9}`))
		close(handlerDone)
	}()

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handleLine blocked on a duplicate response for an already-buffered id")
	}

	select {
	case resp := <-respCh:
		if string(resp.Result) != `{"timestamp":1}` {
			t.Fatalf("resp.Result = %s, want the FIRST response's payload", resp.Result)
		}
	default:
		t.Fatal("expected the first response to still be delivered")
	}
}

// TestHandleLineRoutesResponseToPendingCall pins handleLine's other
// branch: a line carrying an "id" (a response, not a notification) is
// routed to the matching pending call's channel, not treated as an
// inbound message.
func TestHandleLineRoutesResponseToPendingCall(t *testing.T) {
	tr := newJSONRPCTransport("unused:0", "+15555550199")
	respCh := make(chan rpcResponse, 1)
	tr.pendingMu.Lock()
	tr.pending[7] = respCh
	tr.pendingMu.Unlock()

	tr.handleLine([]byte(`{"jsonrpc":"2.0","result":{"timestamp":123},"id":7}`))

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			t.Fatalf("resp.Error = %+v, want nil", resp.Error)
		}
		if string(resp.Result) != `{"timestamp":123}` {
			t.Fatalf("resp.Result = %s, want {\"timestamp\":123}", resp.Result)
		}
	default:
		t.Fatal("expected the response to be routed to the pending call's channel")
	}
}

// TestHandleLineRoutesErrorResponseToPendingCall pins that a JSON-RPC
// error response (e.g. "not registered") is delivered as an rpcError,
// not silently dropped.
func TestHandleLineRoutesErrorResponseToPendingCall(t *testing.T) {
	tr := newJSONRPCTransport("unused:0", "+15555550199")
	respCh := make(chan rpcResponse, 1)
	tr.pendingMu.Lock()
	tr.pending[3] = respCh
	tr.pendingMu.Unlock()

	tr.handleLine([]byte(`{"jsonrpc":"2.0","error":{"code":-1,"message":"User +15555550199 is not registered."},"id":3}`))

	select {
	case resp := <-respCh:
		if resp.Error == nil || resp.Error.Message != "User +15555550199 is not registered." {
			t.Fatalf("resp.Error = %+v, want the not-registered message", resp.Error)
		}
	default:
		t.Fatal("expected the error response to be routed to the pending call's channel")
	}
}

// --- jsonrpcTransport over a real in-memory TCP listener acting as a
// fake signal-cli daemon: no real signal-cli, no real Signal account,
// but a genuine TCP round trip exercising Run/SendMessage end to end.
// This is the "fake JSON-RPC endpoint" the brief's Step 1 asks for. ---

// fakeDaemon is a minimal signal-cli JSON-RPC daemon stand-in: it
// accepts one connection, decodes each newline-delimited JSON-RPC
// request, and replies according to onRequest.
type fakeDaemon struct {
	listener net.Listener
}

func newFakeDaemon(t *testing.T, onRequest func(req rpcRequest) rpcMessage) *fakeDaemon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &fakeDaemon{listener: ln}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var req rpcRequest
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				continue
			}
			resp := onRequest(req)
			b, _ := json.Marshal(resp)
			_, _ = conn.Write(append(b, '\n'))
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return d
}

func (d *fakeDaemon) addr() string { return d.listener.Addr().String() }

// TestJSONRPCTransportSendMessageSuccess exercises SendMessage end to
// end over a real TCP connection to fakeDaemon, confirming the request
// this transport writes is well-formed and its response is correctly
// matched back by id.
func TestJSONRPCTransportSendMessageSuccess(t *testing.T) {
	var gotMethod string
	var gotParams json.RawMessage
	daemon := newFakeDaemon(t, func(req rpcRequest) rpcMessage {
		gotMethod = req.Method
		b, _ := json.Marshal(req.Params)
		gotParams = b
		if req.Method == "send" {
			return rpcMessage{JSONRPC: "2.0", Result: json.RawMessage(`{"timestamp":999}`), ID: &req.ID}
		}
		return rpcMessage{JSONRPC: "2.0", Result: json.RawMessage(`{}`), ID: &req.ID}
	})

	tr := newJSONRPCTransport(daemon.addr(), "+15555550199")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- tr.Run(ctx) }()

	// Give Run a moment to dial and issue subscribeReceive before we
	// send, so gotMethod below reflects the "send" call, not the
	// startup "subscribeReceive" one racing it.
	time.Sleep(50 * time.Millisecond)

	if err := tr.SendMessage(context.Background(), "+15555550100", "hello"); err != nil {
		t.Fatalf("SendMessage() = %v, want nil", err)
	}
	if gotMethod != "send" {
		t.Fatalf("daemon last saw method %q, want \"send\"", gotMethod)
	}
	var params map[string]any
	if err := json.Unmarshal(gotParams, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["account"] != "+15555550199" || params["message"] != "hello" {
		t.Fatalf("send params = %+v, want account=+15555550199 message=hello", params)
	}

	cancel()
	<-runDone
}

// TestJSONRPCTransportSendMessageSurfacesDaemonError pins that a
// JSON-RPC error response (the "not registered" shape this task's own
// dry run is expected to hit against an unlinked account) comes back
// out of SendMessage as a Go error, not silently swallowed or hung.
func TestJSONRPCTransportSendMessageSurfacesDaemonError(t *testing.T) {
	daemon := newFakeDaemon(t, func(req rpcRequest) rpcMessage {
		return rpcMessage{JSONRPC: "2.0", Error: &rpcError{Code: -1, Message: "User +15555550199 is not registered."}, ID: &req.ID}
	})

	tr := newJSONRPCTransport(daemon.addr(), "+15555550199")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- tr.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	err := tr.SendMessage(context.Background(), "+15555550100", "hello")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("SendMessage() error = %v, want it to carry \"not registered\"", err)
	}

	cancel()
	<-runDone
}

// TestJSONRPCTransportForwardsReceiveNotificationOverRealConnection is
// the end-to-end counterpart of TestHandleLineForwardsReceiveNotification:
// the fake daemon pushes an unsolicited "receive" line with no request
// behind it, and Events() must yield the translated Event.
func TestJSONRPCTransportForwardsReceiveNotificationOverRealConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var req rpcRequest
			if json.Unmarshal(scanner.Bytes(), &req) == nil && req.Method == "subscribeReceive" {
				resp, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", Result: json.RawMessage(`1`), ID: &req.ID})
				_, _ = conn.Write(append(resp, '\n'))
				// Push an unsolicited receive notification right after
				// subscribing, exactly like a real daemon would the
				// moment a message arrives.
				notif, _ := json.Marshal(rpcMessage{
					JSONRPC: "2.0",
					Method:  "receive",
					Params:  json.RawMessage(`{"envelope":{"sourceNumber":"+15555550100","dataMessage":{"message":"hi"}}}`),
				})
				_, _ = conn.Write(append(notif, '\n'))
			}
		}
	}()

	tr := newJSONRPCTransport(ln.Addr().String(), "+15555550199")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- tr.Run(ctx) }()

	select {
	case ev := <-tr.Events():
		if ev.Source != "+15555550100" || ev.Text != "hi" {
			t.Fatalf("Event = %+v, want Source=+15555550100 Text=hi", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the receive notification to arrive on Events()")
	}

	cancel()
	<-runDone
}

// TestJSONRPCTransportConcurrentSendMessageDoesNotInterleaveWrites pins
// that concurrent SendMessage calls (Adapter.Send is documented as
// callable outside handle's own synchronous cycle — see
// ports/channels.Channel's doc comment, so concurrent callers are an
// anticipated case, not a hypothetical one) never have their JSON-RPC
// request lines' bytes interleaved on the wire: every line the fake
// daemon receives must decode as exactly one well-formed request
// carrying exactly one of the messages this test sent, never a
// corrupted merge of two.
func TestJSONRPCTransportConcurrentSendMessageDoesNotInterleaveWrites(t *testing.T) {
	const n = 50

	var mu sync.Mutex
	seen := map[string]bool{}
	var malformedLines int

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var req rpcRequest
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				mu.Lock()
				malformedLines++
				mu.Unlock()
				continue
			}
			var params map[string]any
			_ = json.Unmarshal(mustMarshal(req.Params), &params)
			if req.Method == "subscribeReceive" {
				resp, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", Result: json.RawMessage(`1`), ID: &req.ID})
				_, _ = conn.Write(append(resp, '\n'))
				continue
			}
			msg, _ := params["message"].(string)
			mu.Lock()
			seen[msg] = true
			mu.Unlock()
			resp, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", Result: json.RawMessage(`{"timestamp":1}`), ID: &req.ID})
			_, _ = conn.Write(append(resp, '\n'))
		}
	}()

	tr := newJSONRPCTransport(ln.Addr().String(), "+15555550199")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- tr.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- tr.SendMessage(context.Background(), "+15555550100", fmt.Sprintf("message-%d", i))
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("SendMessage() = %v, want nil", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if malformedLines != 0 {
		t.Fatalf("daemon saw %d malformed/corrupted lines, want 0 (interleaved writes)", malformedLines)
	}
	if len(seen) != n {
		t.Fatalf("daemon saw %d distinct messages, want %d — a write was corrupted or lost", len(seen), n)
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("message-%d", i)] {
			t.Fatalf("daemon never saw message-%d intact", i)
		}
	}

	cancel()
	<-runDone
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// TestJSONRPCTransportRunFailsFastOnUnreachableDaemon pins Run's one
// genuinely fatal setup failure (see the package doc comment): a dial
// that can never succeed must return promptly, not hang.
func TestJSONRPCTransportRunFailsFastOnUnreachableDaemon(t *testing.T) {
	// 127.0.0.1:1 is the reserved "no listener" TCP port — connection
	// refused immediately, no real network dependency.
	tr := newJSONRPCTransport("127.0.0.1:1", "+15555550199")
	err := tr.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to return an error for an unreachable daemon")
	}
}

// TestJSONRPCTransportRunClosesEventsOnDialFailure pins a fix:
// cmd/jlp/main.go calls Adapter.Start with context.Background() (never
// cancelled), and Start's own dispatch goroutine is only released by
// EITHER ctx.Done() OR Events() closing (ports/channels.Channel's own
// "Events... closed once Run returns" contract). A dial failure that
// returned early without closing t.events would leak that goroutine
// forever, every single time the sidecar isn't reachable at boot — a
// realistic case, since docker-compose.yml has no startup ordering
// between the app and the signal-cli service. Reading from Events()
// after a failed Run must return immediately with ok=false, not hang.
func TestJSONRPCTransportRunClosesEventsOnDialFailure(t *testing.T) {
	tr := newJSONRPCTransport("127.0.0.1:1", "+15555550199")
	if err := tr.Run(context.Background()); err == nil {
		t.Fatal("expected Run to return an error for an unreachable daemon")
	}

	select {
	case _, ok := <-tr.Events():
		if ok {
			t.Fatal("Events() yielded a value, want the channel simply closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events() did not close after a failed dial — a dispatch goroutine reading it would leak forever")
	}
}
