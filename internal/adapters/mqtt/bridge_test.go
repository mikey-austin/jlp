package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	vocabdomain "github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// --- fakes ---

// fakeMQTTClient is an in-memory mqttClient double: Publish just
// records what it was given (no real broker), Subscribe records its
// filter/handler so a test can drive the handler directly.
type fakeMQTTClient struct {
	mu sync.Mutex

	connectCalls int
	published    []publishedMsg
	subscribed   []string
	publishErr   error
}

type publishedMsg struct {
	topic   string
	payload []byte
}

func newFakeMQTTClient() *fakeMQTTClient { return &fakeMQTTClient{} }

func (f *fakeMQTTClient) Connect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectCalls++
	return nil
}

func (f *fakeMQTTClient) Publish(topic string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, publishedMsg{topic: topic, payload: payload})
	return f.publishErr
}

func (f *fakeMQTTClient) Subscribe(topicFilter string, _ func(topic string, payload []byte)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed = append(f.subscribed, topicFilter)
	return nil
}

func (f *fakeMQTTClient) Disconnect() {}

func (f *fakeMQTTClient) publishedMessages() []publishedMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publishedMsg, len(f.published))
	copy(out, f.published)
	return out
}

// fakeEventStore is a minimal storage.LearningEventRepository double —
// only Append is exercised by learning.Recorder in these tests.
type fakeEventStore struct {
	appended []event.LearningEvent
}

func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	f.appended = append(f.appended, ev)
	return nil
}
func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return f.appended, nil
}
func (f *fakeEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	return f.appended, nil
}

// fakeVocabRepo is a minimal storage.VocabularyRepository double: only
// UpsertOnLookup is exercised (handleIngest's only write path); the
// rest exist solely to satisfy the interface.
type fakeVocabRepo struct {
	mu      sync.Mutex
	upserts int
}

func (f *fakeVocabRepo) UpsertOnLookup(_ context.Context, identity learner.IdentityID, expression, reading, meaning, source, _ string, kind vocabdomain.Kind, _ string, _ time.Time) (vocabdomain.Item, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	return vocabdomain.Item{ID: "item-1", IdentityID: identity, Expression: expression, Reading: reading, Meaning: meaning, Source: source, Kind: kind}, false, nil
}
func (f *fakeVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabdomain.Item, error) {
	return nil, nil
}
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabdomain.Item, error) {
	return nil, nil
}
func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	return nil, nil
}
func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabdomain.BankEntry, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabdomain.Item, error) {
	return nil, nil
}
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	return 0, nil
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	return nil
}

func (f *fakeVocabRepo) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upserts
}

// fakeIdentityRepo is a minimal storage.IdentityRepository double: Get
// fails for any identity not explicitly put.
type fakeIdentityRepo struct {
	known map[learner.IdentityID]learner.Identity
}

func newFakeIdentityRepo() *fakeIdentityRepo {
	return &fakeIdentityRepo{known: map[learner.IdentityID]learner.Identity{}}
}
func (f *fakeIdentityRepo) put(id learner.Identity) { f.known[id.ID] = id }
func (f *fakeIdentityRepo) Upsert(_ context.Context, id learner.Identity) error {
	f.put(id)
	return nil
}
func (f *fakeIdentityRepo) Get(_ context.Context, id learner.IdentityID) (learner.Identity, error) {
	got, ok := f.known[id]
	if !ok {
		return learner.Identity{}, errors.New("identity not found")
	}
	return got, nil
}
func (f *fakeIdentityRepo) ListIdentities(context.Context) ([]learner.Identity, error) {
	out := make([]learner.Identity, 0, len(f.known))
	for _, id := range f.known {
		out = append(out, id)
	}
	return out, nil
}

// newTestVocabService builds a real *vocabulary.Service (the brief's
// NewBridge signature takes the concrete type, not an interface) over
// repo, backed by a real learning.Recorder writing to an in-memory
// event store and bus — the same shape production wiring uses, just
// with fakes standing in for postgres.
func newTestVocabService(repo *fakeVocabRepo) *vocabulary.Service {
	recorder := learning.NewRecorder(&fakeEventStore{}, inprocbus.New())
	return vocabulary.NewService(repo, recorder)
}

// --- topic mapping ---

// TestTopicForEveryAllTypesEntry pins the exact category derivation
// for every event.AllTypes() entry against a hand-written table (not
// a re-derivation of category's own logic), so a bug that e.g. always
// returned the whole type string as "category" would be caught rather
// than trivially passing.
func TestTopicForEveryAllTypesEntry(t *testing.T) {
	want := map[event.Type]string{
		event.TypeWritingCreated:              "writing",
		event.TypeWritingUpdated:              "writing",
		event.TypeFeedbackRequested:           "feedback",
		event.TypeCorrectionPresented:         "correction",
		event.TypeCorrectionAccepted:          "correction",
		event.TypeCorrectionRejected:          "correction",
		event.TypeGrammarConceptEncountered:   "grammar",
		event.TypeVocabularyLookedUp:          "vocabulary",
		event.TypeVocabularyProduced:          "vocabulary",
		event.TypeVocabularyProducedCorrectly: "vocabulary",
		event.TypeHintShown:                   "hint",
		event.TypeCorrectionRetried:           "correction",
		event.TypeAnswerRevealed:              "answer",
		event.TypeConfidenceRecorded:          "confidence",
		event.TypeQuizStarted:                 "quiz",
		event.TypeQuizAnswered:                "quiz",
		event.TypeQuizCompleted:               "quiz",
		event.TypeAnkiCardCreated:             "anki",
		event.TypeAnkiCardExported:            "anki",
		event.TypeVocabularyImported:          "vocabulary",
		event.TypeTutorLessonCreated:          "tutor",
		event.TypeTutorLessonCompleted:        "tutor",
		event.TypeConversationTurn:            "conversation",
		event.TypeConversationSummarised:      "conversation",
		event.TypeSpeechTranscribed:           "speech",
		event.TypeContentDeleted:              "content",
		event.TypeContentRestored:             "content",
	}

	all := event.AllTypes()
	if len(all) != len(want) {
		t.Fatalf("event.AllTypes() has %d entries, this test's table has %d — update the table", len(all), len(want))
	}

	for _, ty := range all {
		wantCategory, ok := want[ty]
		if !ok {
			t.Fatalf("event.AllTypes() contains %q, which is not in this test's table", ty)
		}
		gotTopic := topicFor("dev", ty)
		wantTopic := "learner/dev/" + wantCategory + "/" + string(ty)
		if gotTopic != wantTopic {
			t.Errorf("topicFor(%q) = %q, want %q", ty, gotTopic, wantTopic)
		}
	}
}

// --- envelope marshaling ---

func TestEnvelopeMarshalRoundTrip(t *testing.T) {
	sid := session.ID("sess-1")
	ev := event.LearningEvent{
		ID:         "ev-1",
		IdentityID: "dev",
		SessionID:  &sid,
		Type:       event.TypeCorrectionPresented,
		Subject:    "doc-1",
		Evidence:   map[string]any{"concept": "は/が", "count": float64(2)},
		OccurredAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
	}

	payload, err := json.Marshal(newEnvelope(ev))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["id"] != "ev-1" {
		t.Errorf("id = %v, want ev-1", decoded["id"])
	}
	if decoded["identity_id"] != "dev" {
		t.Errorf("identity_id = %v, want dev", decoded["identity_id"])
	}
	if decoded["session_id"] != "sess-1" {
		t.Errorf("session_id = %v, want sess-1", decoded["session_id"])
	}
	if decoded["type"] != string(event.TypeCorrectionPresented) {
		t.Errorf("type = %v, want %s", decoded["type"], event.TypeCorrectionPresented)
	}
	if decoded["subject"] != "doc-1" {
		t.Errorf("subject = %v, want doc-1", decoded["subject"])
	}
	evidence, ok := decoded["evidence"].(map[string]any)
	if !ok || evidence["concept"] != "は/が" {
		t.Errorf("evidence = %+v, want concept=は/が", decoded["evidence"])
	}
	if decoded["occurred_at"] != "2026-08-14T12:00:00Z" {
		t.Errorf("occurred_at = %v, want 2026-08-14T12:00:00Z", decoded["occurred_at"])
	}
}

// TestEnvelopeOmitsNilSessionID pins that a session-less event (nil
// SessionID — plenty of event.AllTypes() entries have no session,
// e.g. vocabulary.looked-up) doesn't put a null session_id on the
// wire.
func TestEnvelopeOmitsNilSessionID(t *testing.T) {
	ev := event.LearningEvent{ID: "ev-2", IdentityID: "dev", Type: event.TypeVocabularyLookedUp, OccurredAt: time.Now().UTC()}
	payload, err := json.Marshal(newEnvelope(ev))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(payload), "session_id") {
		t.Fatalf("payload contains session_id for a session-less event: %s", payload)
	}
}

// --- outbound bridging ---

// TestNewBridgeSubscribesEveryAllTypesEntry pins the brief's "for each
// event type in event.AllTypes(), bus.Subscribe" requirement directly:
// publishing one event per AllTypes() entry must reach the fake MQTT
// client exactly once each.
func TestNewBridgeSubscribesEveryAllTypesEntry(t *testing.T) {
	fc := newFakeMQTTClient()
	bus := inprocbus.New()
	newBridge(fc, bus, newTestVocabService(&fakeVocabRepo{}), newFakeIdentityRepo())

	for _, ty := range event.AllTypes() {
		if err := bus.Publish(context.Background(), event.LearningEvent{ID: string(ty), IdentityID: "dev", Type: ty}); err != nil {
			t.Fatalf("bus.Publish(%s): %v", ty, err)
		}
	}

	got := fc.publishedMessages()
	if len(got) != len(event.AllTypes()) {
		t.Fatalf("published %d MQTT messages, want %d (one per AllTypes entry)", len(got), len(event.AllTypes()))
	}
}

func TestPublishOutboundUsesMappedTopicAndEnvelope(t *testing.T) {
	fc := newFakeMQTTClient()
	bus := inprocbus.New()
	newBridge(fc, bus, newTestVocabService(&fakeVocabRepo{}), newFakeIdentityRepo())

	ev := event.LearningEvent{ID: "ev-1", IdentityID: "dev", Type: event.TypeCorrectionPresented, Subject: "doc-1", OccurredAt: time.Now().UTC()}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("bus.Publish: %v", err)
	}

	got := fc.publishedMessages()
	if len(got) != 1 {
		t.Fatalf("published %d messages, want 1", len(got))
	}
	if got[0].topic != "learner/dev/correction/correction.presented" {
		t.Fatalf("topic = %q", got[0].topic)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got[0].payload, &decoded); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if decoded["subject"] != "doc-1" {
		t.Fatalf("payload subject = %v, want doc-1", decoded["subject"])
	}
}

// TestPublishOutboundErrorDoesNotPropagate pins the brief's "outbound
// publish errors are logged, never propagate" contract: a client.
// Publish failure must not turn bus.Publish itself into an error the
// original writing/feedback caller would see.
func TestPublishOutboundErrorDoesNotPropagate(t *testing.T) {
	fc := newFakeMQTTClient()
	fc.publishErr = errors.New("broker unreachable")
	bus := inprocbus.New()
	newBridge(fc, bus, newTestVocabService(&fakeVocabRepo{}), newFakeIdentityRepo())

	err := bus.Publish(context.Background(), event.LearningEvent{ID: "ev-1", IdentityID: "dev", Type: event.TypeCorrectionPresented})
	if err != nil {
		t.Fatalf("bus.Publish returned %v, want nil — MQTT publish failures must never propagate", err)
	}
}

// --- inbound ingestion ---

func TestHandleIngestKnownIdentitySucceeds(t *testing.T) {
	identities := newFakeIdentityRepo()
	identities.put(learner.Identity{ID: "dev"})
	repo := &fakeVocabRepo{}
	b := newBridge(newFakeMQTTClient(), inprocbus.New(), newTestVocabService(repo), identities)

	payload := []byte(`{"type":"vocabulary.lookup","expression":"取り組む","reading":"とりくむ","definition":"to tackle, to work on"}`)
	b.handleIngest(context.Background(), "learner/dev/vocabulary/ingest", payload)

	if got := repo.upsertCount(); got != 1 {
		t.Fatalf("UpsertOnLookup called %d times, want 1", got)
	}
}

// TestHandleIngestDropsUnknownIdentity pins the brief's exact "unknown
// identity → log and drop" contract: the write must never reach
// vocabulary.Service.Ingest at all.
func TestHandleIngestDropsUnknownIdentity(t *testing.T) {
	identities := newFakeIdentityRepo() // "dev" deliberately never put
	repo := &fakeVocabRepo{}
	b := newBridge(newFakeMQTTClient(), inprocbus.New(), newTestVocabService(repo), identities)

	payload := []byte(`{"type":"vocabulary.lookup","expression":"取り組む","reading":"とりくむ","definition":"to tackle"}`)
	b.handleIngest(context.Background(), "learner/dev/vocabulary/ingest", payload)

	if got := repo.upsertCount(); got != 0 {
		t.Fatalf("UpsertOnLookup called %d times for an unknown identity, want 0", got)
	}
}

// TestHandleIngestDropsMalformedJSON pins "malformed JSON → log and
// drop": even for a known identity, invalid JSON must never reach
// Ingest.
func TestHandleIngestDropsMalformedJSON(t *testing.T) {
	identities := newFakeIdentityRepo()
	identities.put(learner.Identity{ID: "dev"})
	repo := &fakeVocabRepo{}
	b := newBridge(newFakeMQTTClient(), inprocbus.New(), newTestVocabService(repo), identities)

	b.handleIngest(context.Background(), "learner/dev/vocabulary/ingest", []byte(`not json`))

	if got := repo.upsertCount(); got != 0 {
		t.Fatalf("UpsertOnLookup called %d times for malformed JSON, want 0", got)
	}
}

func TestHandleIngestDropsTopicThatDoesNotMatchTheFilter(t *testing.T) {
	identities := newFakeIdentityRepo()
	identities.put(learner.Identity{ID: "dev"})
	repo := &fakeVocabRepo{}
	b := newBridge(newFakeMQTTClient(), inprocbus.New(), newTestVocabService(repo), identities)

	b.handleIngest(context.Background(), "learner/dev/writing/ingest", []byte(`{}`))

	if got := repo.upsertCount(); got != 0 {
		t.Fatalf("UpsertOnLookup called %d times for a non-matching topic, want 0", got)
	}
}

func TestIdentityFromIngestTopic(t *testing.T) {
	cases := []struct {
		topic    string
		wantID   learner.IdentityID
		wantOK   bool
		testName string
	}{
		{"learner/dev/vocabulary/ingest", "dev", true, "valid"},
		{"learner//vocabulary/ingest", "", false, "empty identity"},
		{"learner/dev/vocabulary", "", false, "too short"},
		{"learner/dev/writing/ingest", "", false, "wrong category"},
		{"learner/dev/vocabulary/lookup", "", false, "wrong action"},
		{"other/dev/vocabulary/ingest", "", false, "wrong prefix"},
	}
	for _, c := range cases {
		t.Run(c.testName, func(t *testing.T) {
			gotID, gotOK := identityFromIngestTopic(c.topic)
			if gotID != c.wantID || gotOK != c.wantOK {
				t.Errorf("identityFromIngestTopic(%q) = (%q, %v), want (%q, %v)", c.topic, gotID, gotOK, c.wantID, c.wantOK)
			}
		})
	}
}

// --- Start ---

func TestStartConnectsAndSubscribesIngestFilter(t *testing.T) {
	fc := newFakeMQTTClient()
	b := newBridge(fc, inprocbus.New(), newTestVocabService(&fakeVocabRepo{}), newFakeIdentityRepo())

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fc.connectCalls != 1 {
		t.Fatalf("Connect called %d times, want 1", fc.connectCalls)
	}
	if len(fc.subscribed) != 1 || fc.subscribed[0] != ingestTopicFilter {
		t.Fatalf("subscribed = %v, want [%s]", fc.subscribed, ingestTopicFilter)
	}
}

// --- pahoClient bounded-wait regression (self-review CRITICAL fix) ---
//
// paho v1.5.1 has a specific trap: once a connected client loses its
// connection with AutoReconnect enabled, it sits in a "reconnecting"
// state where IsConnected() still reports true, but a QoS1 Publish
// issued in that state returns a Token that NEVER has
// setError/flowComplete called on it — Wait() blocks forever. The
// fakes below reproduce exactly that Token shape (WaitTimeout
// deterministically times out, nothing ever completes it) so
// pahoClient.Publish/Subscribe's ioTimeout bound is proven against the
// real trap shape without needing a live broker outage.

// hangingToken simulates a paho Token that never completes — WaitTimeout
// always reports "timed out" (false), matching what a real token stuck
// in the reconnecting-state trap does. Wait() intentionally blocks
// forever too (mirroring the real bug precisely), which is safe here
// only because pahoClient.Publish/Subscribe never call it — they call
// WaitTimeout exclusively, which is the whole point of this fix.
type hangingToken struct{}

func (hangingToken) Wait() bool { select {} }

func (hangingToken) WaitTimeout(time.Duration) bool { return false }
func (hangingToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (hangingToken) Error() error                   { return nil }

// hangingPahoClient is a minimal paho.Client double: Publish and
// Subscribe both return hangingToken{}; every other method is left to
// the embedded nil paho.Client, so calling any of them (which
// pahoClient.Publish/Subscribe never do) panics loudly rather than
// silently returning a zero value — a future change accidentally
// depending on more of the interface fails fast in this test rather
// than passing for the wrong reason.
type hangingPahoClient struct{ paho.Client }

func (hangingPahoClient) Publish(string, byte, bool, interface{}) paho.Token {
	return hangingToken{}
}
func (hangingPahoClient) Subscribe(string, byte, paho.MessageHandler) paho.Token {
	return hangingToken{}
}

// testTimeout is how long these regression tests wait for
// pahoClient.Publish/Subscribe to return before concluding the fix
// regressed and the call is genuinely hanging — generous slack over
// ioTimeout so this never flakes under load, while still being far
// short of "the test suite hangs forever" if the bug reappears.
const testTimeout = ioTimeout + 3*time.Second

// TestPahoClientPublishReturnsErrorRatherThanHangingOnANeverCompletingToken
// is the CRITICAL self-review fix's core regression test: before this
// fix, pahoClient.Publish called token.Wait() unconditionally, which
// for a token like hangingToken never returns — this test would hang
// the whole `go test` process forever on the old code. It now calls
// WaitTimeout(ioTimeout) instead, so Publish must return a non-nil
// error within ioTimeout even for a token that never completes.
func TestPahoClientPublishReturnsErrorRatherThanHangingOnANeverCompletingToken(t *testing.T) {
	c := &pahoClient{client: hangingPahoClient{}}
	done := make(chan error, 1)
	go func() { done <- c.Publish("learner/dev/correction/correction.presented", []byte("{}")) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Publish returned nil for a token that never completes, want a timeout error")
		}
	case <-time.After(testTimeout):
		t.Fatal("Publish did not return within ioTimeout+slack for a token that never completes — it hung, reproducing the paho v1.5.1 reconnecting-state trap")
	}
}

func TestPahoClientSubscribeReturnsErrorRatherThanHangingOnANeverCompletingToken(t *testing.T) {
	c := &pahoClient{client: hangingPahoClient{}}
	done := make(chan error, 1)
	go func() { done <- c.Subscribe(ingestTopicFilter, func(string, []byte) {}) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Subscribe returned nil for a token that never completes, want a timeout error")
		}
	case <-time.After(testTimeout):
		t.Fatal("Subscribe did not return within ioTimeout+slack for a token that never completes — it hung, reproducing the paho v1.5.1 reconnecting-state trap")
	}
}

// ListPage delegates to List and truncates. This package does not page;
// implemented rather than stubbed so it returns real rows if it ever
// starts. See storage.VocabularyRepository.ListPage.
func (f *fakeVocabRepo) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabdomain.Item, storage.VocabularyCursor, error) {
	all, err := f.List(ctx, identity, filter)
	if err != nil || len(all) <= limit {
		return all, storage.VocabularyCursor{}, err
	}
	page := all[:limit]
	last := page[len(page)-1]
	return page, storage.VocabularyCursor{LastEvent: last.LastEvent, ID: last.ID}, nil
}

// ListRecentUnpracticed is 練習's word-drill source; no test in this
// package drills words, so reaching it means a wiring mistake.
func (r *fakeVocabRepo) ListRecentUnpracticed(context.Context, learner.IdentityID, time.Time, int) ([]vocabdomain.Item, error) {
	panic("not used by these tests")
}

// GetByIDs resolves due words for 練習; the practice double below is the
// only one that needs real behaviour.
func (r *fakeVocabRepo) GetByIDs(context.Context, learner.IdentityID, []string) ([]vocabdomain.Item, error) {
	panic("not used by these tests")
}

// LatestExamples backs 練習's cloze drills.
func (r *fakeVocabRepo) LatestExamples(context.Context, learner.IdentityID, []string) (map[string]string, error) {
	panic("not used by these tests")
}

// RecordExample stores a generated example sentence.
func (r *fakeVocabRepo) RecordExample(context.Context, learner.IdentityID, string, string, storage.ExampleOrigin, time.Time) error {
	panic("not used by these tests")
}
