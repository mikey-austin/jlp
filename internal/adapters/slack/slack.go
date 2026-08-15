// Package slack implements Phase 4 Task 4's Slack channel adapter
// (ports/channels.Channel, PRD §20/§20.1) over Socket Mode
// (slack-go/slack/socketmode): Socket Mode dials OUT to Slack, so
// running this adapter needs no public ingress — no new
// docker-compose.yml service, no reverse-proxy route, appropriate for a
// LAN deployment.
//
// Adapter itself carries NO Slack-API wire-format knowledge beyond the
// tiny Event struct below: every actual slack-go/slackevents type is
// decoded inside socketModeTransport (production only, at the bottom of
// this file), so slack_test.go exercises event -> ports/channels.
// Inbound translation, reply -> Send, and ack/error handling entirely
// over a fake transport — no network, no real Slack connection ever
// attempted in tests.
//
// Reconnect/error handling: socketmode.Client.RunContext already
// retries the underlying WebSocket connection on its own (the slack-go
// equivalent of paho's AutoReconnect that internal/adapters/mqtt relies
// on — see that package's own doc comment for the same pattern against
// a different library); Adapter additionally never lets a single inbound
// message's handling (an application error, a failed reply) propagate
// out of dispatch — every failure past the initial connection is logged
// and dropped, never fatal, so one bad message, one down channel, can
// never take the rest of the app with it (cmd/jlp/main.go runs Start on
// its own goroutine specifically so that a genuinely fatal Start error —
// an invalid token, say — only ends this one channel, not the process).
package slack

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"

	goslack "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/mikeyaustin/jlp/internal/ports/channels"
)

// Event is one inbound Slack message, already stripped of every
// Slack-API wire-format detail Adapter itself doesn't need. ack, when
// non-nil, is transport's own callback to acknowledge the underlying
// Socket Mode request — nil for a fake test event that carries no such
// requirement.
type Event struct {
	UserID    string
	ChannelID string
	// ThreadTS is the Slack thread timestamp this message belongs to,
	// "" if it's a top-level channel message rather than a threaded
	// reply.
	ThreadTS string
	Text     string
	ack      func()
}

// transport abstracts socketmode.Client down to exactly what Adapter
// needs: a channel of inbound Event plus a way to post a reply. This is
// the same shape internal/adapters/mqtt's mqttClient interface takes
// over paho (see that package's own doc comment) — it lets slack_test.go
// inject a fake and exercise Adapter's own logic (event -> Inbound,
// reply -> Send, ack-then-handle ordering, error handling) without ever
// dialing Slack. socketModeTransport, at the bottom of this file, is the
// only production implementation.
type transport interface {
	// Run starts the connection and blocks until ctx is done or a fatal
	// SETUP error occurs (e.g. an invalid token rejected on the very
	// first handshake) — never merely on a transient disconnect, which
	// the underlying client retries on its own (see the package doc
	// comment).
	Run(ctx context.Context) error
	// Events yields every inbound Event; closed once Run returns.
	Events() <-chan Event
	// PostMessage sends text into channelID, threaded under threadTS
	// when non-empty.
	PostMessage(ctx context.Context, channelID, threadTS, text string) error
}

// Adapter implements ports/channels.Channel over Slack Socket Mode.
type Adapter struct {
	transport transport
}

// New builds a production Adapter dialing Slack with appToken (the
// Socket Mode app-level token, "xapp-...") and botToken (the bot token,
// "xoxb-..."). Construction never dials anything — see
// newSocketModeTransport; nothing is attempted until Start is called.
func New(appToken, botToken string) *Adapter {
	return newAdapter(newSocketModeTransport(appToken, botToken))
}

// newAdapter is New's transport-injectable core — slack_test.go calls
// this directly with a fake transport.
func newAdapter(t transport) *Adapter {
	return &Adapter{transport: t}
}

// Name satisfies ports/channels.Channel — "slack", the exact value this
// adapter's Inbound.Channel always carries, and what
// application/channel.Service's APP_CHANNELS_ALLOWFROM entries key on.
func (a *Adapter) Name() string { return "slack" }

// Start begins the Socket Mode connection and, for every inbound Event,
// calls handle and sends its Outbound reply back (see handleEvent).
// Each event is handled on its own goroutine so one slow handle call
// (an AI-backed correction) can't stall the next inbound message. Start
// itself blocks on transport.Run, returning only when ctx is done or Run
// hits a fatal setup error.
func (a *Adapter) Start(ctx context.Context, handle func(ctx context.Context, in channels.Inbound) (channels.Outbound, error)) error {
	go a.dispatch(ctx, handle)
	return a.transport.Run(ctx)
}

// dispatch's own recover is belt-and-suspenders on top of handleEvent's
// (below): dispatch itself does little beyond read-select-spawn, but a
// panic anywhere in this loop — today or after a future change — must
// not take the process down any more than one in handleEvent may,
// mirroring cmd/jlp/summary.go's summaryJob (its own per-job recover
// alongside newSummaryCron's outer cron.Recover chain).
func (a *Adapter) dispatch(ctx context.Context, handle func(ctx context.Context, in channels.Inbound) (channels.Outbound, error)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("slack: dispatch loop panicked", "panic", r, "stack", string(debug.Stack()))
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

// handleEvent acks ev (if it carries an ack callback) BEFORE calling
// handle: Slack requires an ack within a few seconds of receiving a
// Socket Mode request or it re-delivers the same event, and handle's own
// work (an AI-backed correction) can comfortably exceed that — acking
// first, then processing, avoids a slow correction turning into a
// duplicate delivery. A blank Text (a system/edit event transport chose
// to forward anyway) is a no-op: nothing to correct, no reply to send.
// Every failure past this point — handle itself erroring, or the
// eventual Send failing — is logged and dropped, never propagated: one
// bad message must never end dispatch's loop, let alone Start's.
//
// handle fans out into application/channel.Service, which in turn
// reaches the feedback/practice services, the teacher/drill agents, and
// the live AI adapter — a panic anywhere along that path would, with no
// recover, unwind this goroutine and crash the ENTIRE process (Go
// terminates the whole program on an unrecovered goroutine panic, not
// just the offending goroutine), taking the web UI down with it. The
// HTTP surface already contains exactly this class of panic via
// internal/adapters/http/server.go's recover middleware; this mirrors
// that same containment for the channel adapter — and,
// belt-and-suspenders, the same defer/recover shape
// cmd/jlp/summary.go's summaryJob and internal/tools/registry.go's
// Invoke already use elsewhere in this codebase for the identical
// reason.
func (a *Adapter) handleEvent(ctx context.Context, ev Event, handle func(ctx context.Context, in channels.Inbound) (channels.Outbound, error)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("slack: handling an inbound event panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if ev.ack != nil {
		ev.ack()
	}
	if strings.TrimSpace(ev.Text) == "" {
		return
	}

	in := channels.Inbound{
		Channel:    "slack",
		ExternalID: ev.UserID,
		Text:       ev.Text,
		ThreadID:   threadKey(ev.ChannelID, ev.ThreadTS),
	}
	out, err := handle(ctx, in)
	if err != nil {
		slog.Error("slack: handle inbound message", "err", err)
		return
	}
	if strings.TrimSpace(out.Text) == "" {
		return
	}
	if err := a.Send(ctx, out); err != nil {
		slog.Error("slack: send reply", "err", err)
	}
}

// Send posts out back to Slack: out.ThreadID is threadKey's own
// encoding (see that function), decoded back into the channel ID
// PostMessage needs plus an optional thread TS. ports/channels.Outbound
// carries no separate channel field, so this round-trip through
// ThreadID — set on the way in by handleEvent, echoed straight back by
// every caller of Handle — is how a Slack reply finds its way back to
// the right channel/thread.
func (a *Adapter) Send(ctx context.Context, out channels.Outbound) error {
	channelID, threadTS := parseThreadKey(out.ThreadID)
	if channelID == "" {
		return errors.New("slack: Outbound.ThreadID does not carry a channel id")
	}
	return a.transport.PostMessage(ctx, channelID, threadTS, out.Text)
}

// threadKey/parseThreadKey encode/decode (channelID, threadTS) into the
// single string ports/channels.Inbound.ThreadID and Outbound.ThreadID
// carry.
func threadKey(channelID, threadTS string) string {
	if threadTS == "" {
		return channelID
	}
	return channelID + "|" + threadTS
}

func parseThreadKey(key string) (channelID, threadTS string) {
	if before, after, ok := strings.Cut(key, "|"); ok {
		return before, after
	}
	return key, ""
}

// --- production transport ---

// socketModeTransport is transport's only production implementation,
// wrapping github.com/slack-go/slack/socketmode.
type socketModeTransport struct {
	client *socketmode.Client
	web    *goslack.Client
	events chan Event
}

// newSocketModeTransport never dials anything — nothing is attempted
// until Run calls the embedded socketmode.Client.RunContext.
func newSocketModeTransport(appToken, botToken string) *socketModeTransport {
	web := goslack.New(botToken, goslack.OptionAppLevelToken(appToken))
	client := socketmode.New(web)
	return &socketModeTransport{client: client, web: web, events: make(chan Event, 32)}
}

func (t *socketModeTransport) Events() <-chan Event { return t.events }

func (t *socketModeTransport) PostMessage(ctx context.Context, channelID, threadTS, text string) error {
	opts := []goslack.MsgOption{goslack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, goslack.MsgOptionTS(threadTS))
	}
	_, _, err := t.web.PostMessageContext(ctx, channelID, opts...)
	return err
}

// Run starts consuming t.client.Events on its own goroutine (translating
// each into this package's own Event shape — see consume) and blocks on
// RunContext, which owns the actual WebSocket connection AND its own
// reconnect-on-drop retry loop; Run itself never retries anything, it
// only returns what RunContext returns.
func (t *socketModeTransport) Run(ctx context.Context) error {
	go t.consume(ctx)
	return t.client.RunContext(ctx)
}

// consume drains t.client.Events, translating connection-lifecycle
// events into log lines (visibility into RunContext's own silent
// reconnect handling — see the package doc comment) and Events API
// message events into this package's own Event, forwarded on t.events.
// Every unrecognized or malformed payload is logged and dropped, never
// panics — this defensiveness is what lets Start's contract ("never
// returns except on a fatal setup failure") actually hold.
func (t *socketModeTransport) consume(ctx context.Context) {
	defer close(t.events)
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-t.client.Events:
			if !ok {
				return
			}
			t.handleSocketEvent(evt)
		}
	}
}

func (t *socketModeTransport) handleSocketEvent(evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		slog.Info("slack: connecting")
	case socketmode.EventTypeConnectionError:
		slog.Warn("slack: connection error, retrying", "data", evt.Data)
	case socketmode.EventTypeConnected:
		slog.Info("slack: connected")
	case socketmode.EventTypeDisconnect:
		slog.Warn("slack: disconnected, reconnecting")
	case socketmode.EventTypeIncomingError, socketmode.EventTypeErrorBadMessage, socketmode.EventTypeErrorWriteFailed:
		slog.Warn("slack: socket mode error", "type", evt.Type, "data", evt.Data)
	case socketmode.EventTypeEventsAPI:
		t.handleEventsAPI(evt)
	}
}

// handleEventsAPI decodes evt.Data as an Events API payload and forwards
// a message/app-mention event onto t.events — see the two case
// branches' own comments for exactly what gets filtered and why. Every
// other inner event type (reactions, channel changes, ...) is
// acknowledged and dropped: application/channel.Service's routing is
// text-message-shaped only.
func (t *socketModeTransport) handleEventsAPI(evt socketmode.Event) {
	outer, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		slog.Warn("slack: events_api payload had an unexpected shape, dropping")
		return
	}
	ack := func() {
		if evt.Request != nil {
			if err := t.client.Ack(*evt.Request); err != nil {
				slog.Warn("slack: ack failed", "err", err)
			}
		}
	}

	switch inner := outer.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		// bot_message/message_changed/message_deleted/etc all carry a
		// non-empty SubType, and any message posted by a bot (including
		// this one, replying to a previous message) carries a non-empty
		// BotID — see MessageEvent's own doc comment. Forwarding either
		// would let the bot's own replies, or another bot's messages,
		// reach application/channel.Service as if a learner had sent
		// them — acked (so Slack doesn't keep re-delivering it) but
		// otherwise dropped.
		//
		// A SubType drop is logged (unlike the BotID case, which is the
		// routine "don't talk to yourself" path and would just be noise
		// on every one of this bot's own replies): SubType also covers
		// file_share/thread_broadcast/me_message — a learner attaching a
		// photo to their Japanese text, say — where silently producing no
		// reply at all would otherwise leave no trail to diagnose why
		// (independent review, Minor 7).
		if inner.SubType != "" {
			slog.Warn("slack: dropping message with a subtype, no reply will be sent", "subtype", inner.SubType, "channel", inner.Channel)
			ack()
			return
		}
		if inner.BotID != "" {
			ack()
			return
		}
		t.events <- Event{UserID: inner.User, ChannelID: inner.Channel, ThreadTS: inner.ThreadTimeStamp, Text: inner.Text, ack: ack}
	case *slackevents.AppMentionEvent:
		if inner.BotID != "" {
			ack()
			return
		}
		t.events <- Event{UserID: inner.User, ChannelID: inner.Channel, ThreadTS: inner.ThreadTimeStamp, Text: stripMention(inner.Text), ack: ack}
	default:
		ack()
	}
}

// stripMention removes a leading "<@U0123ABC>" app-mention token Slack
// prepends to an app_mention event's Text, so
// application/channel.Service sees the same plain message text a DM or
// channel message would carry.
func stripMention(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<@") {
		return text
	}
	if idx := strings.Index(text, ">"); idx != -1 {
		return strings.TrimSpace(text[idx+1:])
	}
	return text
}
