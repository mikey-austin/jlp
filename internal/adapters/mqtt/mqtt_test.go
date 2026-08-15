//go:build integration

package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
