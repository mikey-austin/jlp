// Package signal implements Phase 4 Task 5's Signal channel adapter
// (ports/channels.Channel, PRD §20/§20.1) over a `signal-cli` sidecar
// running in JSON-RPC daemon mode on a plain TCP socket
// (docker-compose.yml's "signal" profile — see deploy/signal/README.md
// for the one-time device-link step and this package's own honesty
// caveat about what could and couldn't be verified without the user's
// phone).
//
// Wire protocol: signal-cli's `daemon --tcp host:port` mode speaks
// JSON-RPC 2.0 over newline-delimited JSON on one persistent
// connection, in BOTH directions at once — this adapter's own request/
// response calls (a `send`, a `subscribeReceive`) share the same
// connection as signal-cli's unsolicited `receive` notifications for
// inbound messages, distinguished by whether a decoded line carries an
// "id" (a response to match against a pending call) or a "method"
// (either "receive", forwarded as an Event, or something else, logged
// and dropped). The sidecar is started WITHOUT a `-a` account of its
// own (see config.Signal's doc comment), so every call this adapter
// makes names Number as the JSON-RPC "account" param.
//
// Adapter itself carries no wire-format knowledge beyond the tiny
// Event struct below and the request/response shapes jsonrpcTransport
// builds at the bottom of this file — signal_test.go exercises event ->
// ports/channels.Inbound translation, reply -> Send, and panic/error
// containment entirely over a fake transport (no network), and
// jsonrpcTransport's own wire encoding/decoding against a real
// in-memory TCP listener acting as a fake signal-cli daemon (still no
// real Signal account, no real network beyond localhost) — mirroring
// internal/adapters/slack's own two-layer test split (see that
// package's doc comment).
//
// Failure handling follows ports/channels.Channel's documented
// contract exactly like internal/adapters/slack: Start blocks and
// returns only on a genuinely fatal setup failure (the initial TCP dial
// failing); every failure past that — a bad inbound line, a failed
// send, handle erroring or even panicking — is logged and dropped,
// never fatal, via the same defer/recover shape slack.go's
// dispatch/handleEvent use (cmd/jlp/main.go runs Start on its own
// goroutine for the same reason Slack's Start is: one channel going
// down must never take the rest of the app with it).
package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/channels"
)

// Event is one inbound Signal message, already stripped of signal-cli's
// own JSON-RPC envelope shape.
type Event struct {
	// Source is the sender's own E.164 phone number
	// (envelope.sourceNumber, falling back to envelope.source) — Signal
	// has no separate channel/thread concept for a 1:1 conversation, so
	// this doubles as both ports/channels.Inbound.ExternalID and
	// ThreadID (see handleEvent below), the same way a Slack DM's
	// channel ID alone (no ThreadTS) already does in that package.
	Source string
	Text   string
}

// transport abstracts the signal-cli JSON-RPC connection down to
// exactly what Adapter needs, mirroring internal/adapters/slack's own
// transport interface (see that package's doc comment for why): it
// lets signal_test.go inject a fake and exercise Adapter's own logic
// (event -> Inbound, reply -> Send, error/panic handling) without ever
// dialing a real signal-cli daemon. jsonrpcTransport, at the bottom of
// this file, is the only production implementation.
type transport interface {
	// Run dials the daemon and blocks until ctx is done or a fatal
	// SETUP error occurs (the initial dial failing) — never merely on a
	// transient disconnect after that (see the package doc comment).
	Run(ctx context.Context) error
	// Events yields every inbound Event; closed once Run returns.
	Events() <-chan Event
	// SendMessage performs one JSON-RPC "send" call to recipient and
	// waits for its matching response, returning any JSON-RPC error the
	// daemon reports (e.g. "not registered" for an unlinked account) as
	// a plain Go error.
	SendMessage(ctx context.Context, recipient, text string) error
}

// Adapter implements ports/channels.Channel over a signal-cli JSON-RPC
// daemon.
type Adapter struct {
	transport transport
}

// New builds a production Adapter dialing rpcAddr (host:port, e.g.
// "signal-cli:6006") and naming number as the JSON-RPC "account" param
// on every call. Construction never dials anything — see
// newJSONRPCTransport; nothing is attempted until Start is called.
func New(rpcAddr, number string) *Adapter {
	return newAdapter(newJSONRPCTransport(rpcAddr, number))
}

// newAdapter is New's transport-injectable core — signal_test.go calls
// this directly with a fake transport.
func newAdapter(t transport) *Adapter {
	return &Adapter{transport: t}
}

// Name satisfies ports/channels.Channel — "signal", the exact value
// this adapter's Inbound.Channel always carries, and what
// application/channel.Service's APP_CHANNELS_ALLOWFROM entries key on.
func (a *Adapter) Name() string { return "signal" }

// Start begins the JSON-RPC connection and, for every inbound Event,
// calls handle and sends its Outbound reply back (see handleEvent).
// Each event is handled on its own goroutine so one slow handle call
// can't stall the next inbound message — same shape as
// internal/adapters/slack.Adapter.Start's own doc comment.
func (a *Adapter) Start(ctx context.Context, handle func(ctx context.Context, in channels.Inbound) (channels.Outbound, error)) error {
	go a.dispatch(ctx, handle)
	return a.transport.Run(ctx)
}

// dispatch's own recover is belt-and-suspenders on top of handleEvent's
// (below) — see internal/adapters/slack.Adapter.dispatch's own doc
// comment for why both layers matter; this mirrors that exactly, and
// the same defer/recover shape cmd/jlp/summary.go's summaryJob and
// internal/tools/registry.go's Invoke already use elsewhere.
func (a *Adapter) dispatch(ctx context.Context, handle func(ctx context.Context, in channels.Inbound) (channels.Outbound, error)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("signal: dispatch loop panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-a.transport.Events():
			if !ok {
				return
			}
			go a.handleEvent(ctx, ev, handle)
		}
	}
}

// handleEvent translates ev into a channels.Inbound, calls handle, and
// sends the reply back. A blank Text is a no-op: nothing to correct, no
// reply to send. Every failure past this point — handle erroring, or
// the eventual Send failing — is logged and dropped, never propagated:
// one bad message must never end dispatch's loop, let alone Start's.
// The recover here is the primary containment for a panic anywhere
// along handle's own call chain (application/channel.Service and
// everything downstream of it) — see
// internal/adapters/slack.Adapter.handleEvent's own doc comment for the
// full "why" (Go crashes the entire process on an unrecovered goroutine
// panic, not just the offending goroutine).
func (a *Adapter) handleEvent(ctx context.Context, ev Event, handle func(ctx context.Context, in channels.Inbound) (channels.Outbound, error)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("signal: handling an inbound event panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if strings.TrimSpace(ev.Text) == "" {
		return
	}

	in := channels.Inbound{
		Channel:    "signal",
		ExternalID: ev.Source,
		Text:       ev.Text,
		ThreadID:   ev.Source,
	}
	out, err := handle(ctx, in)
	if err != nil {
		slog.Error("signal: handle inbound message", "err", err)
		return
	}
	if strings.TrimSpace(out.Text) == "" {
		return
	}
	if err := a.Send(ctx, out); err != nil {
		slog.Error("signal: send reply", "err", err)
	}
}

// Send posts out back to Signal: out.ThreadID carries the recipient's
// E.164 number (see handleEvent, which sets it from Event.Source on the
// way in — the same round-trip-through-ThreadID pattern
// internal/adapters/slack.Adapter.Send's own doc comment describes for
// Slack's channel ID).
func (a *Adapter) Send(ctx context.Context, out channels.Outbound) error {
	if out.ThreadID == "" {
		return fmt.Errorf("signal: Outbound.ThreadID does not carry a recipient number")
	}
	return a.transport.SendMessage(ctx, out.ThreadID, out.Text)
}

// --- production transport ---

const (
	// dialTimeout bounds the initial TCP dial only — Run's one genuinely
	// fatal-setup failure (see the package doc comment); nothing bounds
	// reconnection after that because this transport, like
	// internal/adapters/mqtt's pahoClient, doesn't attempt one on its
	// own — see jsonrpcTransport.Run's own doc comment.
	dialTimeout = 10 * time.Second
	// callTimeout bounds how long SendMessage (and Run's own
	// subscribeReceive call) waits for a matching JSON-RPC response
	// before giving up — signal-cli itself has no notion of a
	// asynchronous ack the way Slack's Socket Mode does, so an
	// unbounded wait here would hang forever if a response line is ever
	// dropped or malformed. Generous for a LAN sidecar call.
	callTimeout = 15 * time.Second
	// eventsBufferSize matches internal/adapters/slack's own Event
	// channel buffer — enough slack that a burst of inbound messages
	// doesn't block the read loop while dispatch drains it.
	eventsBufferSize = 32
)

// jsonrpcTransport is transport's only production implementation,
// wrapping a plain net.Conn to the signal-cli daemon.
type jsonrpcTransport struct {
	addr   string
	number string

	// connMu guards conn and writing to it: SendMessage's write and
	// readLoop's read run on different goroutines, and more than one
	// caller could call SendMessage concurrently (Adapter.Send is part
	// of ports/channels.Channel's public contract, callable from
	// outside handle's own synchronous cycle per that interface's doc
	// comment).
	connMu sync.Mutex
	conn   net.Conn

	events chan Event

	nextID int64 // atomic

	pendingMu sync.Mutex
	pending   map[int64]chan rpcResponse
}

// newJSONRPCTransport never dials anything — nothing is attempted
// until Run is called, matching internal/adapters/mqtt's
// newPahoClient/internal/adapters/slack's newSocketModeTransport own
// "construction never dials" contract.
func newJSONRPCTransport(addr, number string) *jsonrpcTransport {
	return &jsonrpcTransport{
		addr:    addr,
		number:  number,
		events:  make(chan Event, eventsBufferSize),
		pending: make(map[int64]chan rpcResponse),
	}
}

// Run dials t.addr — the one fatal setup failure Start's contract
// allows to actually return an error (see the package doc comment) —
// then starts the read loop and blocks until ctx is done. Unlike
// internal/adapters/mqtt's paho client or Slack's socketmode.Client,
// nothing here retries a dropped connection on its own: a signal-cli
// sidecar dying is expected to be handled by the container orchestrator
// restarting it (docker-compose.yml has no restart policy override for
// the "signal" profile, so this matches Docker's own default), and
// cmd/jlp/main.go already runs Start on its own goroutine, logging
// (never os.Exit-ing) whatever error Run eventually returns — so a
// dead connection degrades this ONE channel, not the process, exactly
// like every other adapter's own contract.
func (t *jsonrpcTransport) Run(ctx context.Context) error {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fmt.Errorf("signal: dial %s: %w", t.addr, err)
	}
	t.connMu.Lock()
	t.conn = conn
	t.connMu.Unlock()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// Belt-and-suspenders, same as every other spawned goroutine in
		// this codebase's adapters (dispatch/handleEvent above,
		// internal/adapters/slack's own dispatch/handleEvent,
		// cmd/jlp/summary.go's summaryJob, internal/tools/registry.go's
		// Invoke): a panic anywhere in readLoop — a malformed line
		// tripping something json.Unmarshal's own type didn't already
		// guard against, say — must never take down the whole process
		// just because it happened on this goroutine rather than
		// dispatch's. Recovering here still leaves readLoop's own
		// return path (io.EOF/read error) as the normal, expected way
		// this goroutine ends; this only guards the abnormal one.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("signal: read loop panicked", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		t.readLoop(conn)
	}()

	// subscribeReceive is signal-cli's own explicit opt-in to streaming
	// `receive` notifications on this connection — issued here, not
	// left to the daemon's own --receive-mode flag, so this adapter
	// behaves the same regardless of how the sidecar's command line is
	// configured. An error here (e.g. "not registered" against an
	// unlinked account — exactly the honesty-clause outcome this task's
	// sidecar is expected to hit) is logged, never fatal: it just means
	// no inbound Event will ever arrive until the account is linked
	// (see deploy/signal/README.md), the same "config accepted, feature
	// stays dormant until credentials actually work" posture as
	// everything else in this codebase gated by an empty config value.
	if err := t.subscribeReceive(ctx); err != nil {
		slog.Warn("signal: subscribeReceive failed, no inbound messages will be delivered until the account is linked", "err", err)
	}

	select {
	case <-ctx.Done():
	case <-readDone:
	}
	_ = conn.Close()
	<-readDone
	close(t.events)
	return nil
}

func (t *jsonrpcTransport) Events() <-chan Event { return t.events }

// SendMessage builds and sends a JSON-RPC "send" request and waits for
// its response — see callTimeout's doc comment for why this is bounded.
func (t *jsonrpcTransport) SendMessage(ctx context.Context, recipient, text string) error {
	_, err := t.call(ctx, "send", map[string]any{
		"account":   t.number,
		"recipient": []string{recipient},
		"message":   text,
	})
	return err
}

// subscribeReceive issues the "subscribeReceive" JSON-RPC call — see
// Run's own comment for why this is called unconditionally rather than
// relying on the daemon's --receive-mode flag.
func (t *jsonrpcTransport) subscribeReceive(ctx context.Context) error {
	_, err := t.call(ctx, "subscribeReceive", map[string]any{"account": t.number})
	return err
}

// call performs one request/response JSON-RPC round trip: it registers
// a pending response channel keyed by a freshly allocated id, writes
// the marshaled request line, and waits for readLoop to deliver the
// matching response (or ctx/callTimeout to expire, or the connection to
// be gone).
func (t *jsonrpcTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&t.nextID, 1)
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: id}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("signal: marshal %s request: %w", method, err)
	}

	respCh := make(chan rpcResponse, 1)
	t.pendingMu.Lock()
	t.pending[id] = respCh
	t.pendingMu.Unlock()
	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, id)
		t.pendingMu.Unlock()
	}()

	// connMu is held across the Write itself, not just the read of
	// t.conn: two concurrent calls (Adapter.Send is documented as
	// callable outside handle's own synchronous cycle — see
	// ports/channels.Channel's doc comment — so concurrent callers are
	// an anticipated, not merely theoretical, case) must never have
	// their two request lines' bytes interleaved on the wire, which an
	// unlocked conn.Write from two goroutines could otherwise risk —
	// net.Conn permits concurrent use (Read/Write/Close from different
	// goroutines is documented-safe), but does NOT guarantee two
	// concurrent Write calls stay byte-atomic relative to each other,
	// and this protocol is framed by newlines, not length-prefixed, so
	// an interleaved write would corrupt more than just the two calls
	// involved.
	t.connMu.Lock()
	conn := t.conn
	if conn == nil {
		t.connMu.Unlock()
		return nil, fmt.Errorf("signal: %s: not connected", method)
	}
	_, err = conn.Write(append(line, '\n'))
	t.connMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("signal: write %s request: %w", method, err)
	}

	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, fmt.Errorf("signal: %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("signal: %s: timed out waiting for a response after %s", method, callTimeout)
	}
}

// readLoop reads newline-delimited JSON-RPC messages off conn until it
// closes or errors, dispatching each to either a pending call's
// response channel (an "id" present — see call above) or, for a
// "receive" notification, translateEnvelope's result forwarded on
// t.events. Every malformed or unrecognized line is logged and
// dropped, never panics — the same defensiveness
// internal/adapters/slack's own consume loop documents for the same
// "Start's contract must actually hold" reason.
func (t *jsonrpcTransport) readLoop(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		t.handleLine(line)
	}
}

// handleLine decodes one line and routes it — split out of readLoop
// purely so signal_test.go can drive it directly with hand-built lines
// without needing a real net.Conn for every case (the in-memory-TCP
// tests exercise readLoop itself end to end; these exercise the
// decode/route logic in isolation, same split as
// internal/adapters/slack's Adapter-level vs. socketModeTransport-level
// tests).
func (t *jsonrpcTransport) handleLine(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.Warn("signal: dropping malformed JSON-RPC line", "err", err)
		return
	}

	if msg.Method == "receive" {
		ev, ok := translateEnvelope(msg.Params)
		if !ok {
			slog.Warn("signal: dropping a receive notification with an unexpected envelope shape")
			return
		}
		select {
		case t.events <- ev:
		default:
			slog.Warn("signal: events channel full, dropping an inbound message")
		}
		return
	}

	if msg.ID != nil {
		t.pendingMu.Lock()
		ch, ok := t.pending[*msg.ID]
		t.pendingMu.Unlock()
		if !ok {
			// A response to a call nobody is waiting for any more (e.g.
			// it already timed out) — logged at Warn, not Error: this is
			// expected under a slow/timed-out call, not a protocol bug.
			slog.Warn("signal: response to an unknown or already-completed request id, dropping", "id", *msg.ID)
			return
		}
		ch <- rpcResponse{Result: msg.Result, Error: msg.Error}
		return
	}

	slog.Warn("signal: dropping a JSON-RPC line that is neither a receive notification nor a response", "method", msg.Method)
}

// --- wire shapes ---

// rpcRequest is the outbound JSON-RPC 2.0 request shape signal-cli's
// daemon expects, e.g.
// {"jsonrpc":"2.0","method":"send","params":{"account":"+1...","recipient":["+1..."],"message":"hi"},"id":1}.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      int64  `json:"id"`
}

// rpcMessage is the generic shape of any line arriving FROM the daemon
// — either a response to a call this transport made (ID + Result/Error
// set, Method empty) or an unsolicited notification (Method set, ID
// nil). Params is decoded lazily via json.RawMessage since its shape
// depends entirely on Method.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	ID      *int64          `json:"id,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcResponse is what call's pending channel carries — just the two
// fields it actually needs out of rpcMessage.
type rpcResponse struct {
	Result json.RawMessage
	Error  *rpcError
}

// receiveParams/envelope/dataMessage mirror ONLY the subset of
// signal-cli's own "receive" notification shape this adapter reads —
// see the package doc comment's link to the JSON-RPC wire protocol. A
// notification this adapter doesn't care about (a receipt, a typing
// indicator, a sync message, an envelope with no dataMessage at all —
// e.g. a delivery receipt for this bot's own outgoing message) decodes
// with DataMessage nil and is dropped by translateEnvelope below,
// exactly like Slack's own bot-message/subtype filtering in
// socketModeTransport.handleEventsAPI.
type receiveParams struct {
	Envelope struct {
		Source       string `json:"source"`
		SourceNumber string `json:"sourceNumber"`
		DataMessage  *struct {
			Message string `json:"message"`
		} `json:"dataMessage"`
	} `json:"envelope"`
}

// translateEnvelope decodes params (a "receive" notification's own
// params object) into an Event, or ok=false for anything this adapter
// doesn't forward: malformed JSON, or an envelope with no dataMessage
// (a receipt/typing/sync notification, not a text message a learner
// sent).
func translateEnvelope(params json.RawMessage) (Event, bool) {
	var p receiveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return Event{}, false
	}
	if p.Envelope.DataMessage == nil {
		return Event{}, false
	}
	source := p.Envelope.SourceNumber
	if source == "" {
		source = p.Envelope.Source
	}
	if source == "" {
		return Event{}, false
	}
	return Event{Source: source, Text: p.Envelope.DataMessage.Message}, true
}
