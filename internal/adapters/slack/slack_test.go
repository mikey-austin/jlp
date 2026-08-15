package slack

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/mikeyaustin/jlp/internal/ports/channels"
)

// fakeTransport is transport's test double — no network, no real Slack
// connection. Run blocks (like the real transport.Run does) until ctx
// is done, and every PostMessage call is captured for assertions.
type fakeTransport struct {
	events chan Event

	mu    sync.Mutex
	posts []postCall

	runErr error
}

type postCall struct {
	channelID, threadTS, text string
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{events: make(chan Event, 8)}
}

func (f *fakeTransport) Run(ctx context.Context) error {
	<-ctx.Done()
	return f.runErr
}

func (f *fakeTransport) Events() <-chan Event { return f.events }

func (f *fakeTransport) PostMessage(_ context.Context, channelID, threadTS, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, postCall{channelID, threadTS, text})
	return nil
}

func (f *fakeTransport) Posts() []postCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postCall(nil), f.posts...)
}

// waitFor polls cond every 5ms for up to 2s — long enough to be
// reliable in CI, short enough that a genuine failure doesn't stall the
// suite.
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
	if a.Name() != "slack" {
		t.Fatalf("Name() = %q, want \"slack\"", a.Name())
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

	acked := false
	ft.events <- Event{
		UserID: "U123", ChannelID: "C1", ThreadTS: "",
		Text: "とても面白いでした",
		ack:  func() { acked = true },
	}

	waitFor(t, func() bool { return len(ft.Posts()) == 1 })
	posts := ft.Posts()
	if posts[0].channelID != "C1" || posts[0].threadTS != "" || posts[0].text != "reply text" {
		t.Fatalf("PostMessage call = %+v, want {C1, \"\", \"reply text\"}", posts[0])
	}

	mu.Lock()
	in := gotIn
	mu.Unlock()
	if in.Channel != "slack" || in.ExternalID != "U123" || in.Text != "とても面白いでした" || in.ThreadID != "C1" {
		t.Fatalf("handle received Inbound = %+v, want Channel=slack ExternalID=U123 ThreadID=C1", in)
	}
	if !acked {
		t.Fatal("event was not acked")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v after ctx cancellation, want nil", err)
	}
}

// TestStartThreadsReplyUnderExistingThreadTS pins threadKey's encoding:
// a message that arrived inside an existing Slack thread must get its
// reply posted into that same thread, not a fresh top-level message.
func TestStartThreadsReplyUnderExistingThreadTS(t *testing.T) {
	ft := newFakeTransport()
	a := newAdapter(ft)
	handle := func(_ context.Context, in channels.Inbound) (channels.Outbound, error) {
		return channels.Outbound{ThreadID: in.ThreadID, Text: "reply"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Start(ctx, handle) }()

	ft.events <- Event{UserID: "U1", ChannelID: "C1", ThreadTS: "169900.0001", Text: "hi"}

	waitFor(t, func() bool { return len(ft.Posts()) == 1 })
	post := ft.Posts()[0]
	if post.channelID != "C1" || post.threadTS != "169900.0001" {
		t.Fatalf("PostMessage call = %+v, want channelID=C1 threadTS=169900.0001", post)
	}
}

// TestStartSkipsBlankText: a system/edit event with empty text must
// never reach handle, and never produce a reply.
func TestStartSkipsBlankText(t *testing.T) {
	ft := newFakeTransport()
	a := newAdapter(ft)
	handleCalled := false
	handle := func(_ context.Context, in channels.Inbound) (channels.Outbound, error) {
		handleCalled = true
		return channels.Outbound{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Start(ctx, handle) }()

	acked := false
	ft.events <- Event{UserID: "U1", ChannelID: "C1", Text: "   ", ack: func() { acked = true }}

	waitFor(t, func() bool { return acked })
	time.Sleep(20 * time.Millisecond) // give a wrongly-dispatched handle call a chance to happen
	if handleCalled {
		t.Fatal("handle was called for a blank-text event")
	}
	if len(ft.Posts()) != 0 {
		t.Fatalf("PostMessage calls = %d, want 0 for a blank-text event", len(ft.Posts()))
	}
}

// TestStartLogsHandleErrorAndKeepsDispatching pins "reconnect/error
// paths in the adapter are logged, never fatal": a handle error for one
// event must not stop dispatch from processing the next one, and must
// not make Start return.
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

	ft.events <- Event{UserID: "U1", ChannelID: "C1", Text: "boom"}
	ft.events <- Event{UserID: "U1", ChannelID: "C1", Text: "fine"}

	waitFor(t, func() bool { return len(ft.Posts()) == 1 })
	if got := ft.Posts()[0].text; got != "ok" {
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

// TestSendRejectsThreadIDWithNoChannel pins Send's defensive check: a
// caller that (incorrectly) sends an Outbound whose ThreadID doesn't
// carry a channel id gets an error, not a PostMessage call with an
// empty channelID.
func TestSendRejectsThreadIDWithNoChannel(t *testing.T) {
	ft := newFakeTransport()
	a := newAdapter(ft)
	if err := a.Send(context.Background(), channels.Outbound{ThreadID: "", Text: "hi"}); err == nil {
		t.Fatal("expected an error for an empty ThreadID")
	}
	if len(ft.Posts()) != 0 {
		t.Fatalf("PostMessage calls = %d, want 0", len(ft.Posts()))
	}
}

func TestThreadKeyRoundTrip(t *testing.T) {
	cases := []struct{ channelID, threadTS string }{
		{"C1", ""},
		{"C1", "169900.0001"},
	}
	for _, c := range cases {
		key := threadKey(c.channelID, c.threadTS)
		gotChannel, gotTS := parseThreadKey(key)
		if gotChannel != c.channelID || gotTS != c.threadTS {
			t.Errorf("threadKey(%q,%q) -> parseThreadKey = (%q,%q), want (%q,%q)", c.channelID, c.threadTS, gotChannel, gotTS, c.channelID, c.threadTS)
		}
	}
}

func TestStripMention(t *testing.T) {
	cases := map[string]string{
		"<@U0123ABC> こんにちは":        "こんにちは",
		"<@U0123ABC>":              "",
		"こんにちは":                    "こんにちは",
		"  <@U0123ABC>  hi there ": "hi there",
	}
	for in, want := range cases {
		if got := stripMention(in); got != want {
			t.Errorf("stripMention(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- production transport's Events API decoding (same package, so
// these exercise socketModeTransport.handleEventsAPI directly against
// constructed slackevents payloads — still no network, no real Slack
// connection). ---

func TestHandleEventsAPIForwardsPlainMessage(t *testing.T) {
	tr := newSocketModeTransport("xapp-test", "xoxb-test")
	evt := socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.MessageEvent{User: "U1", Channel: "C1", ThreadTimeStamp: "", Text: "こんにちは"},
			},
		},
	}
	tr.handleEventsAPI(evt)

	select {
	case ev := <-tr.events:
		if ev.UserID != "U1" || ev.ChannelID != "C1" || ev.Text != "こんにちは" {
			t.Fatalf("forwarded Event = %+v, want UserID=U1 ChannelID=C1 Text=こんにちは", ev)
		}
	default:
		t.Fatal("expected a plain message event to be forwarded, nothing was")
	}
}

// TestHandleEventsAPIFiltersBotAndSubtypedMessages is the socket
// transport's own leak-prevention pin: a message carrying a BotID (the
// bot's own reply, or another bot's message) or any SubType
// (message_changed, message_deleted, bot_message, ...) must never reach
// application/channel.Service.
func TestHandleEventsAPIFiltersBotAndSubtypedMessages(t *testing.T) {
	cases := []*slackevents.MessageEvent{
		{User: "U1", Channel: "C1", Text: "hi", BotID: "B1"},
		{User: "U1", Channel: "C1", Text: "hi", SubType: "message_changed"},
	}
	for _, msg := range cases {
		tr := newSocketModeTransport("xapp-test", "xoxb-test")
		evt := socketmode.Event{
			Type: socketmode.EventTypeEventsAPI,
			Data: slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{Data: msg}},
		}
		tr.handleEventsAPI(evt)
		select {
		case ev := <-tr.events:
			t.Fatalf("expected %+v to be filtered, got forwarded as %+v", msg, ev)
		default:
		}
	}
}

func TestHandleEventsAPIForwardsAppMentionWithStrippedText(t *testing.T) {
	tr := newSocketModeTransport("xapp-test", "xoxb-test")
	evt := socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT123> 練習"},
			},
		},
	}
	tr.handleEventsAPI(evt)

	select {
	case ev := <-tr.events:
		if ev.Text != "練習" {
			t.Fatalf("forwarded Event.Text = %q, want the mention token stripped -> \"練習\"", ev.Text)
		}
	default:
		t.Fatal("expected an app_mention event to be forwarded, nothing was")
	}
}

func TestHandleEventsAPIIgnoresUnexpectedPayloadShape(t *testing.T) {
	tr := newSocketModeTransport("xapp-test", "xoxb-test")
	// evt.Data is not a slackevents.EventsAPIEvent at all — must not
	// panic, must not forward anything.
	tr.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: "unexpected"})
	select {
	case ev := <-tr.events:
		t.Fatalf("expected nothing forwarded for a malformed payload, got %+v", ev)
	default:
	}
}
