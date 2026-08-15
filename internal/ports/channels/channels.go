// Package channels is the transport-agnostic channel port (Phase 4 Task
// 4, PRD §20/§20.1): the domain — and, deliberately, application/channel
// too, past the Inbound/Outbound boundary — never learns which
// messaging surface (Slack, Signal, WhatsApp, ...) an interaction
// arrived over beyond the bare Channel string every Inbound carries.
// Every concrete transport (internal/adapters/slack today; Signal/
// WhatsApp in a later task) implements Channel by translating its own
// wire format into Inbound and Outbound back out, so
// application/channel.Service's routing logic (Japanese text →
// correction, "practice"/練習 → drill, "help"/ヘルプ → command list) is
// written exactly once, not once per channel.
package channels

import "context"

// Inbound is one inbound message from a channel, already translated out
// of that channel's own event shape.
type Inbound struct {
	// Channel names the transport this message arrived over — "slack" |
	// "signal" | "whatsapp" — the same string application/config's
	// APP_CHANNELS_ALLOWFROM entries key on (see config.Channels.
	// AllowFrom's doc comment: "<channel>:<external id>=<identity>").
	Channel string
	// ExternalID is the channel's own identifier for the sender (a Slack
	// user ID, a Signal phone number, ...) — opaque to
	// application/channel.Service beyond using it, together with
	// Channel, as the APP_CHANNELS_ALLOWFROM lookup key.
	ExternalID string
	Text       string
	// ThreadID is the channel's own thread/conversation identifier, when
	// it has one (a Slack thread_ts) — echoed back on Outbound.ThreadID
	// so a reply lands in the same thread the inbound message did.
	ThreadID string
}

// Outbound is one reply to send back over the channel a message came
// from.
type Outbound struct{ ThreadID, Text string }

// Channel is a transport adapter: Start begins listening however that
// specific channel does so (Slack's Socket Mode, a future Signal
// poller, a webhook) and calls handle once per inbound message,
// forwarding whatever Outbound handle returns back over the same
// channel — Start itself owns the channel-specific mechanics of "get an
// Outbound back to the sender," not the caller. Start must not return
// except on a genuinely fatal, unrecoverable setup failure (an invalid
// token, say); transient connection loss is the adapter's own job to
// retry and log, never propagated up to end the process (see
// internal/adapters/slack's own doc comment).
//
// Send lets a caller push an Outbound outside handle's synchronous
// req/reply cycle (e.g. a future proactive nudge) — Task 4 wires no
// caller for it yet, but every adapter must still implement it so a
// later caller (Task 7's spaced-retrieval reminders, say) has a
// channel-agnostic way to reach a learner.
type Channel interface {
	Name() string
	Start(ctx context.Context, handle func(ctx context.Context, in Inbound) (Outbound, error)) error
	Send(ctx context.Context, out Outbound) error
}
