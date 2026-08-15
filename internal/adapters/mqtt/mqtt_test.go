//go:build integration

package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// testBrokerURL skips the test unless APP_MQTT_URL is set — the same
// skip-when-unset pattern internal/adapters/postgres/identities_test.go
// uses for APP_DATABASE_URL. `make up-mqtt` starts the compose "mqtt"
// profile (real mosquitto, no host port) and exports
// APP_MQTT_URL=tcp://mosquitto:1883 for the tools container; a plain
// `make test-integration` never sets it, so this test — and only this
// test — stays skipped by default, keeping that target green with no
// mosquitto dependency at all. Run it explicitly with:
//
//	make up-mqtt
//	docker compose run --rm -e APP_MQTT_URL=tcp://mosquitto:1883 tools \
//	    go test -tags integration ./internal/adapters/mqtt/...
func testBrokerURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("APP_MQTT_URL")
	if url == "" {
		t.Skip("APP_MQTT_URL not set — see this file's testBrokerURL doc comment to run against `make up-mqtt`")
	}
	return url
}

// rawSubscribe opens its own bare paho connection — independent of
// the Bridge under test, standing in for an external observer like
// `mosquitto_sub -t 'learner/#' -v` (make mqtt-tap) — and returns a
// channel fed with every message payload received on topicFilter.
func rawSubscribe(t *testing.T, brokerURL, topicFilter string) <-chan []byte {
	t.Helper()
	ch := make(chan []byte, 8)
	opts := paho.NewClientOptions().AddBroker(brokerURL).SetClientID("mqtt-it-sub-" + fmt.Sprint(time.Now().UnixNano()))
	client := paho.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		t.Fatalf("raw subscriber connect: %v", token.Error())
	}
	t.Cleanup(func() { client.Disconnect(250) })
	if token := client.Subscribe(topicFilter, 1, func(_ paho.Client, msg paho.Message) {
		ch <- msg.Payload()
	}); token.Wait() && token.Error() != nil {
		t.Fatalf("raw subscriber subscribe %s: %v", topicFilter, token.Error())
	}
	return ch
}

// rawPublish opens its own bare paho connection and publishes payload
// to topic — standing in for an external reading app (or
// `mosquitto_pub`, what make mqtt-demo uses) publishing an ingest
// payload directly to the broker (PRD §32), entirely independent of
// the Bridge under test.
func rawPublish(t *testing.T, brokerURL, topic string, payload []byte) {
	t.Helper()
	opts := paho.NewClientOptions().AddBroker(brokerURL).SetClientID("mqtt-it-pub-" + fmt.Sprint(time.Now().UnixNano()))
	client := paho.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		t.Fatalf("raw publisher connect: %v", token.Error())
	}
	defer client.Disconnect(250)
	if token := client.Publish(topic, 1, false, payload); token.Wait() && token.Error() != nil {
		t.Fatalf("raw publish %s: %v", topic, token.Error())
	}
}

// TestBridgeOutboundPublishesLearningEventToMosquitto is the brief's
// Step 1 outbound integration scenario: a LearningEvent recorded
// through application/learning.Recorder (the same call every real
// producer in this codebase makes) ends up on the broker, on the
// exact mapped topic, as valid envelope JSON — proven by a completely
// independent raw paho subscriber standing in for `mosquitto_sub`.
func TestBridgeOutboundPublishesLearningEventToMosquitto(t *testing.T) {
	brokerURL := testBrokerURL(t)
	ctx := context.Background()

	bus := inprocbus.New()
	bridge, err := NewBridge(brokerURL, bus, newTestVocabService(&fakeVocabRepo{}), newFakeIdentityRepo())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(bridge.client.Disconnect)

	identity := learner.IdentityID(fmt.Sprintf("mqtt-it-out-%d", time.Now().UnixNano()))
	topic := fmt.Sprintf("learner/%s/correction/correction.presented", identity)
	received := rawSubscribe(t, brokerURL, topic)

	recorder := learning.NewRecorder(&fakeEventStore{}, bus)
	if err := recorder.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeCorrectionPresented,
		Subject:    "doc-mqtt-it",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	select {
	case payload := <-received:
		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("payload not JSON: %v (%s)", err, payload)
		}
		if decoded["subject"] != "doc-mqtt-it" {
			t.Fatalf("subject = %v, want doc-mqtt-it", decoded["subject"])
		}
		if decoded["type"] != string(event.TypeCorrectionPresented) {
			t.Fatalf("type = %v, want %s", decoded["type"], event.TypeCorrectionPresented)
		}
		if decoded["identity_id"] != string(identity) {
			t.Fatalf("identity_id = %v, want %s", decoded["identity_id"], identity)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the bridged event on " + topic)
	}
}

// TestBridgeInboundIngestCreatesVocabularyItem is the brief's Step 1
// inbound integration scenario: a raw MQTT publish to
// learner/{id}/vocabulary/ingest — standing in for an external reading
// app — flows through the real broker into the Bridge's inbound
// subscription and comes out the other side as a vocabulary item, via
// the exact same application/vocabulary.Service.Ingest path
// POST /api/v1/vocabulary/events uses.
func TestBridgeInboundIngestCreatesVocabularyItem(t *testing.T) {
	brokerURL := testBrokerURL(t)
	ctx := context.Background()

	identity := learner.IdentityID(fmt.Sprintf("mqtt-it-in-%d", time.Now().UnixNano()))
	identities := newFakeIdentityRepo()
	identities.put(learner.Identity{ID: identity})
	repo := &fakeVocabRepo{}

	bridge, err := NewBridge(brokerURL, inprocbus.New(), newTestVocabService(repo), identities)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(bridge.client.Disconnect)

	topic := fmt.Sprintf("learner/%s/vocabulary/ingest", identity)
	payload := []byte(`{"type":"vocabulary.lookup","expression":"取り組む","reading":"とりくむ","definition":"to tackle, to work on"}`)
	rawPublish(t, brokerURL, topic, payload)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if repo.upsertCount() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the MQTT ingest to reach vocabulary.Service.Ingest")
}

// TestBridgeInboundDropsUnknownIdentity is the inbound integration
// counterpart of bridge_test.go's TestHandleIngestDropsUnknownIdentity
// — proven here against a real broker round trip rather than a direct
// handleIngest call, so a wiring mistake (e.g. subscribing the wrong
// filter) can't hide the drop behind a test that never actually
// exercised the broker.
func TestBridgeInboundDropsUnknownIdentity(t *testing.T) {
	brokerURL := testBrokerURL(t)
	ctx := context.Background()

	identity := learner.IdentityID(fmt.Sprintf("mqtt-it-unknown-%d", time.Now().UnixNano()))
	repo := &fakeVocabRepo{}

	bridge, err := NewBridge(brokerURL, inprocbus.New(), newTestVocabService(repo), newFakeIdentityRepo() /* identity never put */)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(bridge.client.Disconnect)

	topic := fmt.Sprintf("learner/%s/vocabulary/ingest", identity)
	payload := []byte(`{"type":"vocabulary.lookup","expression":"取り組む","reading":"とりくむ","definition":"to tackle"}`)
	rawPublish(t, brokerURL, topic, payload)

	// Give the broker plenty of time to deliver, then assert nothing
	// landed — there is no positive event to wait on here, only the
	// absence of one.
	time.Sleep(500 * time.Millisecond)
	if got := repo.upsertCount(); got != 0 {
		t.Fatalf("UpsertOnLookup called %d times for an unknown identity over real MQTT, want 0", got)
	}
}

// --- broker-outage regression (self-review CRITICAL fix) ---
//
// This test environment has no docker socket available to actually
// stop the mosquitto container from inside a test, so a real broker
// outage is reproduced instead with a severable TCP proxy sitting in
// front of the real broker: the bridge connects through the proxy,
// the proxy is then severed (stops accepting new connections AND
// kills every connection already piped), and paho's AutoReconnect is
// left retrying against a now-refusing address — the exact
// "reconnecting, and staying that way" state
// pahoClient.Publish/Subscribe's ioTimeout bound exists to survive
// (see that constant's doc comment in mqtt.go). This is the stronger
// of the two proof levels the brief allowed for; see this file's
// TestPahoClientPublishReturnsErrorRatherThanHangingOnANeverCompletingToken
// sibling in bridge_test.go for the unit-level fallback proof against
// a literal never-completing token.

// severableProxy relays TCP connections to target until sever is
// called, at which point it stops accepting new connections and
// forcibly closes every connection already piped — simulating a
// broker that has gone completely unreachable (not just one dropped
// session): any in-flight session dies, and every reconnect attempt
// after that gets connection-refused.
type severableProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	conns []net.Conn
}

func startSeverableProxy(t *testing.T, target string) *severableProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("severableProxy: listen: %v", err)
	}
	p := &severableProxy{ln: ln, target: target}
	go p.acceptLoop()
	t.Cleanup(p.sever)
	return p
}

func (p *severableProxy) addr() string { return "tcp://" + p.ln.Addr().String() }

func (p *severableProxy) acceptLoop() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return // listener closed by sever()
		}
		broker, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, broker)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(broker, client) }()
		go func() { _, _ = io.Copy(client, broker) }()
	}
}

// sever is the point of no return: stop accepting new connections and
// close every connection piped so far. Safe to call more than once
// (t.Cleanup also calls it) — closing an already-closed net.Conn or
// net.Listener just returns an error this test doesn't care about.
func (p *severableProxy) sever() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

// severTestBound is how long TestPahoClientPublishDoesNotHangWhenBroker
// ConnectionIsSevered/TestBridgeRecordDoesNotHangWhenBrokerConnection
// IsSevered wait for their respective calls to return before failing —
// generous slack over ioTimeout, same reasoning as bridge_test.go's
// testTimeout.
const severTestBound = ioTimeout + 3*time.Second

// TestPahoClientPublishDoesNotHangWhenBrokerConnectionIsSevered proves
// the CRITICAL fix end to end against a real (proxied) broker
// connection, not just a synthetic never-completing token: connect
// through the severable proxy, sever it mid-session, and assert
// pahoClient.Publish still returns — with an error — within
// ioTimeout+slack rather than blocking forever in paho's
// "reconnecting" state.
func TestPahoClientPublishDoesNotHangWhenBrokerConnectionIsSevered(t *testing.T) {
	brokerURL := testBrokerURL(t)
	target := strings.TrimPrefix(brokerURL, "tcp://")
	proxy := startSeverableProxy(t, target)

	client, err := newPahoClient(proxy.addr())
	if err != nil {
		t.Fatalf("newPahoClient: %v", err)
	}
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect through proxy: %v", err)
	}
	t.Cleanup(client.Disconnect)

	// Baseline: publishing through the healthy proxy works, so a
	// subsequent failure is provably caused by severing it, not by a
	// broken proxy/connect path.
	baselineTopic := fmt.Sprintf("learner/mqtt-it-sever-baseline-%d/vocabulary/looked-up", time.Now().UnixNano())
	if err := client.Publish(baselineTopic, []byte("{}")); err != nil {
		t.Fatalf("baseline publish through the healthy proxy failed: %v", err)
	}

	proxy.sever()
	// Give paho's connection-loss detection and first failed reconnect
	// attempt a moment to actually land in the "reconnecting" state
	// this test targets — the assertion below doesn't depend on exact
	// timing, only on Publish returning within severTestBound.
	time.Sleep(300 * time.Millisecond)

	severTopic := fmt.Sprintf("learner/mqtt-it-sever-%d/vocabulary/looked-up", time.Now().UnixNano())
	done := make(chan error, 1)
	go func() { done <- client.Publish(severTopic, []byte("{}")) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Publish returned nil after the broker connection was severed, want a timeout/publish error")
		}
	case <-time.After(severTestBound):
		t.Fatal("Publish did not return within ioTimeout+slack after the broker connection was severed — it hung, reproducing the reported production bug live")
	}
}

// TestBridgeRecordDoesNotHangWhenBrokerConnectionIsSevered reproduces
// the exact reported chain end to end: application/learning.Recorder.
// Record (called synchronously from the HTTP request path in
// production) -> ports/events.EventBus.Publish (inprocbus dispatches
// on the calling goroutine) -> Bridge.publishOutbound ->
// pahoClient.Publish, with the broker connection severed mid-session.
// Record must still return promptly and with a NIL error — a lost
// MQTT publish must never fail, let alone hang, the durable event
// write that already succeeded before Publish was even attempted (see
// learning.Recorder.Record's own doc comment on that ordering).
func TestBridgeRecordDoesNotHangWhenBrokerConnectionIsSevered(t *testing.T) {
	brokerURL := testBrokerURL(t)
	ctx := context.Background()
	target := strings.TrimPrefix(brokerURL, "tcp://")
	proxy := startSeverableProxy(t, target)

	bus := inprocbus.New()
	bridge, err := NewBridge(proxy.addr(), bus, newTestVocabService(&fakeVocabRepo{}), newFakeIdentityRepo())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if err := bridge.Start(ctx); err != nil {
		t.Fatalf("Start through proxy: %v", err)
	}
	t.Cleanup(bridge.client.Disconnect)

	proxy.sever()
	time.Sleep(300 * time.Millisecond)

	recorder := learning.NewRecorder(&fakeEventStore{}, bus)
	identity := learner.IdentityID(fmt.Sprintf("mqtt-it-sever-record-%d", time.Now().UnixNano()))
	done := make(chan error, 1)
	go func() {
		done <- recorder.Record(ctx, event.LearningEvent{
			IdentityID: identity,
			Type:       event.TypeCorrectionPresented,
			Subject:    "doc-sever",
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Recorder.Record returned %v, want nil — the durable append must succeed regardless of MQTT's health", err)
		}
	case <-time.After(severTestBound):
		t.Fatal("Recorder.Record hung after the broker connection was severed — this is exactly the reported 'every learning-event write in the app hangs' bug")
	}
}
