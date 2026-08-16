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
// contract exactly like internal/adapters/slack: Start blocks and only
// returns once its context is done. Neither a failed dial (the sidecar
// not reachable yet — docker-compose.yml gives the app no startup
// ordering against it) nor a live connection loss (the sidecar
// restarting, or the connection otherwise dying) is fatal — both are
// logged and retried with a fixed backoff (jsonrpcTransport.Run's own
// doc comment), the same self-healing posture Slack's
// socketmode.Client.RunContext and internal/adapters/mqtt's paho
// AutoReconnect already provide from their own underlying client
// libraries. Every failure at the message level past that — a bad
// inbound line, a failed send, handle erroring or even panicking — is
// logged and dropped, never fatal, via the same defer/recover shape
// slack.go's dispatch/handleEvent use (cmd/jlp/main.go runs Start on
// its own goroutine for the same reason Slack's Start is: one channel
// going down must never take the rest of the app with it).
package signal

import (
	"bufio"
	"bytes"
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
	// Run dials the daemon and blocks until ctx is done — a failed
	// dial, or a live disconnect, is retried rather than returned (see
	// the package doc comment and jsonrpcTransport.Run's own).
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
	// dialTimeout bounds a single TCP dial attempt.
	dialTimeout = 10 * time.Second
	// callTimeout bounds how long SendMessage (and Run's own
	// subscribeReceive call) waits for a matching JSON-RPC response
	// before giving up — signal-cli itself has no notion of a
	// asynchronous ack the way Slack's Socket Mode does, so an
	// unbounded wait here would hang forever if a response line is ever
	// dropped or malformed. Generous for a LAN sidecar call. Also used
	// as the write deadline in call() (see that function's own comment
	// on why an unbounded conn.Write is its own hazard, independent of
	// this response-wait timeout).
	callTimeout = 15 * time.Second
	// reconnectBackoff is how long Run waits between reconnect attempts
	// after a dial failure or a live connection loss — see Run's own
	// doc comment. Fixed rather than exponential: this is a LAN sidecar
	// on the same compose network, not a public endpoint worth being
	// gentle with, and docker-compose.yml gives the app no startup
	// ordering against signal-cli, so a few retries in the first
	// several seconds after boot is the expected, common case, not an
	// edge case worth backing off slowly from.
	reconnectBackoff = 3 * time.Second
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
	// backoff is Run's own reconnect delay, and callTimeout bounds both
	// the write deadline and the response wait in call() — both
	// fields, defaulted from their same-named constants by
	// newJSONRPCTransport, rather than the constants used directly,
	// purely so signal_test.go can shrink them and keep the
	// reconnect/write-deadline tests fast and deterministic (a real
	// stalled-peer write-deadline expiry, tested against net.Pipe,
	// without waiting out the real 15s default) without changing the
	// real defaults any production caller gets.
	backoff     time.Duration
	callTimeout time.Duration

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
		addr:        addr,
		number:      number,
		backoff:     reconnectBackoff,
		callTimeout: callTimeout,
		events:      make(chan Event, eventsBufferSize),
		pending:     make(map[int64]chan rpcResponse),
	}
}

// Run dials t.addr and, once connected, forwards every inbound
// `receive` notification onto Events() until ctx is done — see the
// package doc comment for Start's contract this implements. UNLIKE an
// earlier version of this method, a dial failure or a live connection
// loss (the sidecar restarting, or the connection otherwise dying
// while ctx is still live) is NOT treated as fatal: both are logged and
// retried after reconnectBackoff, exactly the "transient connection
// loss is the adapter's own job to retry and log, never propagated up
// to end the process" contract ports/channels.Channel's own doc
// comment documents — the same policy Slack's own
// socketmode.Client.RunContext and internal/adapters/mqtt's paho
// AutoReconnect already provide for free from their underlying client
// libraries (see that package's own doc comment on the identical
// policy over paho, including retrying the INITIAL connect for the
// exact same "no compose startup ordering" reason). This transport
// dials a raw net.Conn itself with no such library underneath it, so
// it implements the identical retry policy by hand. Run only returns
// once ctx itself is done — a genuinely fatal, unrecoverable setup
// failure (a malformed address net.Dialer itself refuses to attempt)
// is possible only via a config value config.validate() already
// rejects at boot, so is not a live path here in practice; Run does
// not special-case it separately.
func (t *jsonrpcTransport) Run(ctx context.Context) error {
	defer close(t.events)
	for {
		err := t.connectAndServe(ctx)
		if ctx.Err() != nil {
			return nil
		}
		slog.Error("signal: connection lost, reconnecting", "err", err, "backoff", t.backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(t.backoff):
		}
	}
}

// connectAndServe performs ONE dial-and-serve cycle: it blocks until
// either ctx is done (a clean, requested stop — returns nil) or the
// read loop ends on its own while ctx is still live — the connection
// dying, the sidecar restarting, or a line exceeding readLoop's own
// size cap (bufio.Scanner's ErrTooLong) — in which case it returns a
// non-nil error describing why, for Run's own reconnect loop to log
// and act on. It never touches t.events itself; Run's own single
// defer owns closing that, exactly once, regardless of how many
// reconnect cycles happen first.
func (t *jsonrpcTransport) connectAndServe(ctx context.Context) error {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fmt.Errorf("signal: dial %s: %w", t.addr, err)
	}
	t.connMu.Lock()
	t.conn = conn
	t.connMu.Unlock()

	readDone := make(chan struct{})
	var readErr error
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
		readErr = t.readLoop(conn)
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
		// A requested stop: closing conn is what unblocks readLoop's
		// blocking Read, so wait for it to actually finish before
		// returning — whatever error that produces (a "use of closed
		// network connection" read error, typically) is expected and
		// deliberately not reported; it did not happen on its own.
		_ = conn.Close()
		<-readDone
		return nil
	case <-readDone:
		// The read loop ended on its own — ctx is NOT done, so this is
		// an UNREQUESTED end: the connection died, the sidecar
		// restarted, or a line tripped bufio.Scanner's size cap. This
		// must be reported (Important 1, independent review): a
		// permanently-dead-but-silent channel is worse than one that
		// logs and dies, because nothing tells anyone to restart it.
		// Run's own loop is what turns this into a reconnect rather
		// than a real failure.
		_ = conn.Close()
		if readErr != nil {
			return fmt.Errorf("signal: read loop ended: %w", readErr)
		}
		return fmt.Errorf("signal: read loop ended: connection closed by the remote sidecar")
	}
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
	// json.Encoder.Encode, unlike json.Marshal, appends the trailing
	// newline this line-delimited protocol needs as part of the same
	// write into buf — json.Marshal's own returned slice is cap==len,
	// so a separate append(line, '\n') on it (the original shape here)
	// forces a second allocation and copy on every single call this
	// transport ever makes.
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
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
	// A write deadline, independent of callTimeout's own bound on the
	// RESPONSE wait below: without one, a stalled peer that stops
	// reading (TCP send buffer full, connection open) makes conn.Write
	// block indefinitely WHILE connMu IS HELD — every other in-flight
	// or future call() would then queue on the mutex behind it, and
	// since dispatch spawns a fresh handleEvent goroutine per inbound
	// message, each ending in a.Send, those goroutines would accumulate
	// without bound for as long as inbound traffic continued
	// (independent review, Important 2). Reusing callTimeout here
	// rather than a separate constant: a write to a healthy LAN
	// connection is near-instant, so this bound is only ever reached by
	// exactly the pathological-peer case it exists to catch.
	if err := conn.SetWriteDeadline(time.Now().Add(t.callTimeout)); err != nil {
		t.connMu.Unlock()
		return nil, fmt.Errorf("signal: set write deadline: %w", err)
	}
	_, err := conn.Write(buf.Bytes())
	t.connMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("signal: write %s request: %w", method, err)
	}

	timer := time.NewTimer(t.callTimeout)
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
		return nil, fmt.Errorf("signal: %s: timed out waiting for a response after %s", method, t.callTimeout)
	}
}

// readLoop reads newline-delimited JSON-RPC messages off conn until it
// closes or errors, dispatching each to either a pending call's
// response channel (an "id" present — see call above) or, for a
// "receive" notification, translateEnvelope's result forwarded on
// t.events. Every malformed or unrecognized LINE is logged and
// dropped, never panics — the same defensiveness
// internal/adapters/slack's own consume loop documents for the same
// "Start's contract must actually hold" reason; that is a different
// concern from readLoop ITSELF ending, which this now reports (see the
// return value below and connectAndServe's own doc comment) rather
// than swallowing, per the independent review's Important 1: a
// scanner.Scan loop that silently stops (a closed/reset connection, or
// a single line over the 1 MiB cap below tripping bufio.ErrTooLong)
// used to leave this whole channel permanently, silently dead.
//
// The 1 MiB cap itself is deliberately NOT being raised: signal-cli's
// JSON-RPC "receive" notifications never inline attachment bytes — an
// attachment is delivered as a file-path/metadata reference in
// dataMessage.attachments (written to disk by signal-cli itself, read
// separately), never base64 in the envelope — and Signal's own text
// message length cap is on the order of a few KB, so no legitimate
// payload this protocol can produce comes remotely close to 1 MiB. The
// cap exists purely as a hostile/malfunctioning-peer bound, and the
// error path above (report + reconnect) is the correct response to
// ever hitting it, not a bigger buffer.
//
// readLoop's own return value is scanner.Err() — nil on a clean EOF,
// non-nil on an actual I/O error — NOT nil-vs-io.EOF distinguished any
// further: connectAndServe treats either an unrequested loop exit
// (whether the error itself was nil or not) as equally worth
// reconnecting over, since ctx already tells it apart from a requested
// stop.
func (t *jsonrpcTransport) readLoop(conn net.Conn) error {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		t.handleLine(line)
	}
	return scanner.Err()
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
		// Non-blocking, same shape as the receive-notification send
		// above: respCh is buffered exactly 1 (see call), sized for the
		// single well-formed response call expects. A daemon that ever
		// sends two response lines for the same id (a bug, or a
		// retransmit) must not be able to block THIS goroutine — the
		// only one reading every line on the connection, both responses
		// and inbound messages — on a full channel nobody but call's
		// own already-satisfied select will ever drain again; that
		// would stall every other pending call and every future inbound
		// message behind it, not just this one bad line.
		select {
		case ch <- rpcResponse{Result: msg.Result, Error: msg.Error}:
		default:
			slog.Warn("signal: dropping a duplicate or unexpected extra response for request id", "id", *msg.ID)
		}
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
