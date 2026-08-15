// Package mqtt bridges JLP's in-process learning-event bus to MQTT
// (PRD §31-33, §12, §59). It is a transport adapter, nothing more:
// every domain event.LearningEvent published on ports/events.EventBus
// is re-published, QoS1, to "learner/{identity}/{category}/{type}" as
// a transport-independent JSON envelope — the domain type itself gains
// no MQTT/JSON-tag knowledge (Rule 7/8: dependencies point inward,
// domain knows nothing about any one transport). Inbound, the exact
// same wire shape POST /api/v1/vocabulary/events accepts is read back
// off "learner/+/vocabulary/ingest" and routed through the same
// application/vocabulary.Service.Ingest write path — an external
// reading app on the LAN can publish a lookup directly to the broker,
// no HTTP round trip needed (PRD §32's "external reading application
// ingestion").
//
// Failures never propagate into a feedback round. Outbound: a publish
// error is logged and dropped — the event is already durable (see
// application/learning.Recorder, which appends before publishing and
// already absorbs any bus-handler error the same way), MQTT delivery
// is best-effort broadcast, not a second copy of truth. Inbound: a
// message whose topic names an identity Get can't find, or whose
// payload isn't valid JSON, is logged and dropped rather than erroring
// the MQTT client or the identity's vocabulary.
//
// Trust model (PRD §32): the ingest subscription trusts the
// {identity} topic segment outright — anyone who can publish to the
// broker can publish as any identity string. This mirrors
// mosquitto.conf's allow_anonymous LAN-dev posture (see
// deploy/mosquitto/mosquitto.conf): appropriate for a private home
// network, not for a broker exposed past it. A deployment that opens
// MQTT beyond the LAN must add its own broker-level authentication/
// ACLs; this bridge implements none.
package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/events"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	// clientID identifies this bridge's own connection to the broker,
	// distinct from an external reading app or a `mosquitto_sub` tap
	// connected to the same broker.
	clientID = "jlp-mqtt-bridge"
	// qos is used for every publish and subscribe this bridge makes —
	// QoS1 ("at least once"), matching the brief's exact requirement.
	qos = 1
	// ingestTopicFilter is the inbound subscription (PRD §32's
	// "external reading application ingestion"): the `+` wildcard
	// matches exactly one topic segment, the identity.
	ingestTopicFilter = "learner/+/vocabulary/ingest"
	// ingestCategory/ingestAction are ingestTopicFilter's fixed 3rd/4th
	// segments — identityFromIngestTopic checks an inbound message's
	// topic actually matches them (defense in depth: this bridge only
	// ever subscribes ingestTopicFilter, so in practice every message
	// it receives already matches, but the check costs nothing and
	// documents the expected shape rather than assuming it silently).
	ingestCategory = "vocabulary"
	ingestAction   = "ingest"
	// connectTimeout bounds Start's initial Connect — without it, an
	// unreachable broker would hang boot forever rather than failing
	// fast the way every other cmd/jlp/main.go construction step does.
	// Reconnection after a successful initial connect is unbounded,
	// handled by AutoReconnect (see newPahoClient).
	connectTimeout = 10 * time.Second
)

// mqttClient abstracts the paho.mqtt.golang client down to exactly
// what Bridge needs, so unit tests (bridge_test.go) can inject a fake
// and exercise topic mapping, envelope building, and inbound routing
// without a live broker. pahoClient below is the only production
// implementation; mqtt_test.go's build-tag-gated integration test is
// the only test that exercises the real paho client end to end,
// against real mosquitto.
type mqttClient interface {
	Connect() error
	Publish(topic string, payload []byte) error
	Subscribe(topicFilter string, handler func(topic string, payload []byte)) error
	Disconnect()
}

// Bridge wires ports/events.EventBus and an MQTT broker together, both
// directions. See the package doc comment for the exact topic mapping,
// envelope shape, and failure semantics.
type Bridge struct {
	client     mqttClient
	vocab      *vocabulary.Service
	identities storage.IdentityRepository
}

// NewBridge constructs a Bridge over broker url (e.g.
// "tcp://mosquitto:1883") and immediately subscribes an outbound
// handler for every event.AllTypes() entry on bus — that half of the
// bridge needs no broker connection at all (ports/events.EventBus.
// Subscribe is purely in-process), so it happens here rather than
// being deferred to Start, the same way every other bus.Subscribe call
// in cmd/jlp/main.go happens at construction time. Start (below) is
// what actually dials the broker and adds the inbound ingest
// subscription.
//
// The error return is for a broker url that fails to parse; it is NOT
// for an unreachable broker — paho's client is lazy, nothing is dialed
// until Start calls Connect.
func NewBridge(rawURL string, bus events.EventBus, vocab *vocabulary.Service, identities storage.IdentityRepository) (*Bridge, error) {
	client, err := newPahoClient(rawURL)
	if err != nil {
		return nil, err
	}
	return newBridge(client, bus, vocab, identities), nil
}

// newBridge is NewBridge's client-injectable core: bridge_test.go
// calls this directly with a fake mqttClient double so topic mapping,
// envelope building, and inbound ingest routing are all covered by
// fast, broker-free unit tests.
func newBridge(client mqttClient, bus events.EventBus, vocab *vocabulary.Service, identities storage.IdentityRepository) *Bridge {
	b := &Bridge{client: client, vocab: vocab, identities: identities}
	for _, t := range event.AllTypes() {
		bus.Subscribe(t, b.publishOutbound)
	}
	return b
}

// Start connects to the broker and subscribes the inbound ingest
// topic. It returns an error only if the initial Connect or Subscribe
// fails; once connected, paho's AutoReconnect (see newPahoClient)
// handles transient broker outages on its own — Start is not meant to
// be called again after a successful return.
func (b *Bridge) Start(ctx context.Context) error {
	if err := b.client.Connect(); err != nil {
		return fmt.Errorf("mqtt: connect: %w", err)
	}
	if err := b.client.Subscribe(ingestTopicFilter, func(topic string, payload []byte) {
		b.handleIngest(ctx, topic, payload)
	}); err != nil {
		return fmt.Errorf("mqtt: subscribe %s: %w", ingestTopicFilter, err)
	}
	slog.Info("mqtt: bridge started", "ingest_topic", ingestTopicFilter)
	return nil
}

// publishOutbound is bus.Subscribe's handler for every
// event.AllTypes() entry (wired in newBridge): it builds the
// transport-independent envelope and publishes it to
// "learner/{identity}/{category}/{type}". It always returns nil — see
// the package doc comment's failure-semantics paragraph: a marshal or
// publish failure is logged, never returned, so it can never become a
// second failure mode for the writing/feedback round that produced
// the event learning.Recorder already durably appended before
// Publish was even called.
func (b *Bridge) publishOutbound(_ context.Context, ev event.LearningEvent) error {
	topic := topicFor(ev.IdentityID, ev.Type)
	payload, err := json.Marshal(newEnvelope(ev))
	if err != nil {
		slog.Error("mqtt: marshal envelope", "type", ev.Type, "err", err)
		return nil
	}
	if err := b.client.Publish(topic, payload); err != nil {
		slog.Error("mqtt: publish", "topic", topic, "err", err)
	}
	return nil
}

// topicFor builds "learner/{identity}/{category}/{type}" — category
// is t's first dot-segment (e.g. "vocabulary.looked-up" ->
// "vocabulary"), per PRD §32's topic hierarchy.
func topicFor(identity learner.IdentityID, t event.Type) string {
	return fmt.Sprintf("learner/%s/%s/%s", identity, category(t), t)
}

// category returns t's first dot-segment. Every event.AllTypes()
// entry has one — event.go's own naming convention is
// category.subtype[.detail] — so the no-dot branch is defensive only.
func category(t event.Type) string {
	s := string(t)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i]
	}
	return s
}

// envelope is the transport-independent JSON shape MQTT payloads carry
// (PRD §32): {id, identity_id, session_id, type, subject, evidence,
// occurred_at}. It exists specifically so event.LearningEvent itself
// never has to gain json tags or any other MQTT-shaped knowledge (Rule
// 7/8) — this is that translation, kept entirely inside the adapter
// that needs it.
type envelope struct {
	ID         string         `json:"id"`
	IdentityID string         `json:"identity_id"`
	SessionID  *string        `json:"session_id,omitempty"`
	Type       string         `json:"type"`
	Subject    string         `json:"subject"`
	Evidence   map[string]any `json:"evidence,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

func newEnvelope(ev event.LearningEvent) envelope {
	var sessionID *string
	if ev.SessionID != nil {
		s := string(*ev.SessionID)
		sessionID = &s
	}
	return envelope{
		ID:         ev.ID,
		IdentityID: string(ev.IdentityID),
		SessionID:  sessionID,
		Type:       string(ev.Type),
		Subject:    ev.Subject,
		Evidence:   ev.Evidence,
		OccurredAt: ev.OccurredAt,
	}
}

// handleIngest routes one message received on ingestTopicFilter
// through application/vocabulary.Service.Ingest — the exact same
// write path, and wire shape, as POST /api/v1/vocabulary/events (PRD
// §12, §32). See the package doc comment for the unknown-identity/
// malformed-JSON drop semantics; both are logged, neither propagates.
func (b *Bridge) handleIngest(ctx context.Context, topic string, payload []byte) {
	identity, ok := identityFromIngestTopic(topic)
	if !ok {
		slog.Error("mqtt: ingest: topic does not match the ingest filter, dropping", "topic", topic)
		return
	}

	if _, err := b.identities.Get(ctx, identity); err != nil {
		slog.Warn("mqtt: ingest: unknown identity, dropping", "identity", identity, "topic", topic)
		return
	}

	var ev vocabulary.IngestEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		slog.Warn("mqtt: ingest: malformed JSON payload, dropping", "identity", identity, "topic", topic, "err", err)
		return
	}

	if _, err := b.vocab.Ingest(ctx, identity, ev); err != nil {
		slog.Error("mqtt: ingest failed", "identity", identity, "err", err)
	}
}

// identityFromIngestTopic extracts {identity} from
// "learner/{identity}/vocabulary/ingest", validating the other three
// segments match ingestTopicFilter's fixed parts exactly.
func identityFromIngestTopic(topic string) (learner.IdentityID, bool) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "learner" || parts[1] == "" || parts[2] != ingestCategory || parts[3] != ingestAction {
		return "", false
	}
	return learner.IdentityID(parts[1]), true
}

// pahoClient is mqttClient's only production implementation, wrapping
// github.com/eclipse/paho.mqtt.golang.
type pahoClient struct {
	client paho.Client
}

// newPahoClient returns a paho-backed mqttClient dialing rawURL (e.g.
// "tcp://mosquitto:1883"). AutoReconnect is enabled so a broker outage
// after the first successful Start self-heals without this process
// being restarted, per the brief's "reconnect via paho auto-reconnect
// options". Construction itself never dials anything — nothing is
// attempted until Start calls Connect.
func newPahoClient(rawURL string) (mqttClient, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return nil, fmt.Errorf("mqtt: invalid broker url %q: %w", rawURL, err)
	}
	opts := paho.NewClientOptions().
		AddBroker(rawURL).
		SetClientID(clientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectTimeout(connectTimeout)
	return &pahoClient{client: paho.NewClient(opts)}, nil
}

func (c *pahoClient) Connect() error {
	token := c.client.Connect()
	token.Wait()
	return token.Error()
}

func (c *pahoClient) Publish(topic string, payload []byte) error {
	token := c.client.Publish(topic, qos, false, payload)
	token.Wait()
	return token.Error()
}

func (c *pahoClient) Subscribe(topicFilter string, handler func(topic string, payload []byte)) error {
	token := c.client.Subscribe(topicFilter, qos, func(_ paho.Client, msg paho.Message) {
		handler(msg.Topic(), msg.Payload())
	})
	token.Wait()
	return token.Error()
}

func (c *pahoClient) Disconnect() {
	c.client.Disconnect(250)
}
