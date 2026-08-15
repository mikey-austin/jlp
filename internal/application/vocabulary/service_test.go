package vocabulary_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // inprocbus is a port-shaped test double injected via learning.NewRecorder(..., events.EventBus); PRD §75 forbids application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// --- fakes ---

type fakeEventStore struct {
	events []event.LearningEvent
}

func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	f.events = append(f.events, ev)
	return nil
}
func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return f.events, nil
}
func (f *fakeEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	return f.events, nil
}

// fakeVocabRepo is an in-memory storage.VocabularyRepository double,
// identity-scoped like the real postgres adapter: keys combine
// identity+expression (mirroring the UNIQUE(identity_id, expression)
// constraint) and identity+client_event_id (mirroring the partial
// UNIQUE(identity_id, client_event_id) index) so cross-identity data
// never leaks and idempotency is exercised the same way the real
// adapter's transaction does.
type fakeVocabRepo struct {
	items        map[string]*vocabulary.Item // key: identity/expression
	byID         map[string]*vocabulary.Item // key: item id
	clientEvents map[string]string           // key: identity/clientEventID -> item id
	nextID       int

	productions []productionCall
}

type productionCall struct {
	Identity   learner.IdentityID
	ItemID     string
	Successful bool
}

func newFakeVocabRepo() *fakeVocabRepo {
	return &fakeVocabRepo{
		items:        map[string]*vocabulary.Item{},
		byID:         map[string]*vocabulary.Item{},
		clientEvents: map[string]string{},
	}
}

func vocabKey(identity learner.IdentityID, expression string) string {
	return string(identity) + "/" + expression
}

func clientEventKey(identity learner.IdentityID, clientEventID string) string {
	return string(identity) + "/" + clientEventID
}

func (f *fakeVocabRepo) UpsertOnLookup(_ context.Context, identity learner.IdentityID, expression, reading, meaning, source, _ string, kind vocabulary.Kind, clientEventID string, at time.Time) (vocabulary.Item, bool, error) {
	if clientEventID != "" {
		if itemID, ok := f.clientEvents[clientEventKey(identity, clientEventID)]; ok {
			return *f.byID[itemID], true, nil
		}
	}

	k := vocabKey(identity, expression)
	item, ok := f.items[k]
	if !ok {
		f.nextID++
		item = &vocabulary.Item{
			ID:         fmt.Sprintf("vocab-%d", f.nextID),
			IdentityID: identity,
			Expression: expression,
			Reading:    reading,
			Meaning:    meaning,
			Kind:       kind,
			Source:     source,
			FirstSeen:  at,
		}
		f.items[k] = item
		f.byID[item.ID] = item
	}
	item.Lookups++
	item.LastEvent = at
	if reading != "" {
		item.Reading = reading
	}
	if meaning != "" {
		item.Meaning = meaning
	}
	if source != "" {
		item.Source = source
	}

	if clientEventID != "" {
		f.clientEvents[clientEventKey(identity, clientEventID)] = item.ID
	}

	return *item, false, nil
}

func (f *fakeVocabRepo) RecordProduction(_ context.Context, identity learner.IdentityID, itemID string, successful bool, at time.Time) error {
	f.productions = append(f.productions, productionCall{Identity: identity, ItemID: itemID, Successful: successful})
	item, ok := f.byID[itemID]
	if !ok {
		return storage.ErrNotFound
	}
	item.Productions++
	if successful {
		item.SuccessfulProductions++
	}
	item.LastEvent = at
	return nil
}

func (f *fakeVocabRepo) List(_ context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error) {
	var out []vocabulary.Item
	for _, item := range f.items {
		if item.IdentityID != identity {
			continue
		}
		switch filter {
		case "", "looked-up":
			// every item was created by a lookup
		case "produced":
			if item.Productions == 0 {
				continue
			}
		case "activate":
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (f *fakeVocabRepo) AllExpressions(_ context.Context, identity learner.IdentityID) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range f.items {
		if item.IdentityID == identity {
			out[item.Expression] = item.ID
		}
	}
	return out, nil
}

// SeedBank mirrors the real adapter's insert-if-absent contract (see
// storage.VocabularyRepository.SeedBank): a no-op per-entry when
// (identity, expression) already has a row. Not exercised by this
// package's tests — appvocabulary.Service never calls it — but
// implemented for real (not a panic) since it's cheap and keeps this
// fake usable if a future test needs it.
func (f *fakeVocabRepo) SeedBank(_ context.Context, identity learner.IdentityID, entries []vocabulary.BankEntry, at time.Time) error {
	for _, e := range entries {
		k := vocabKey(identity, e.Expression)
		if _, ok := f.items[k]; ok {
			continue
		}
		f.nextID++
		item := &vocabulary.Item{
			ID:         fmt.Sprintf("vocab-%d", f.nextID),
			IdentityID: identity,
			Expression: e.Expression,
			Reading:    e.Reading,
			Meaning:    e.Meaning,
			Kind:       e.Kind,
			Source:     "expression bank",
			FirstSeen:  at,
			LastEvent:  at,
		}
		f.items[k] = item
		f.byID[item.ID] = item
	}
	return nil
}

// ListActivationCandidates is not used by this package's tests —
// appvocabulary.Service never calls it — so it panics if actually
// called, same as the rest of this fake's unused-surface methods.
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	panic("not used by vocabulary service tests")
}

const testIdentity = learner.IdentityID("learner-a")
const testSessionID = session.ID("sess-1")

type harness struct {
	svc    *appvocabulary.Service
	repo   *fakeVocabRepo
	events *fakeEventStore
}

func newHarness() *harness {
	repo := newFakeVocabRepo()
	events := &fakeEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	return &harness{svc: appvocabulary.NewService(repo, rec), repo: repo, events: events}
}

func lookupEvent(expression string) appvocabulary.IngestEvent {
	ev := appvocabulary.IngestEvent{
		Type:       "vocabulary.lookup",
		Expression: expression,
		Reading:    "とりくむ",
		Meaning:    "to tackle, to work on",
	}
	ev.Source.Type = "novel"
	ev.Source.Title = "コンビニ人間"
	return ev
}

// TestIngestHappyPath pins the brief's core contract: a first lookup
// creates an item with Lookups=1, and records a vocabulary.looked-up
// learning event carrying the source.
func TestIngestHappyPath(t *testing.T) {
	h := newHarness()
	item, err := h.svc.Ingest(context.Background(), testIdentity, lookupEvent("取り組む"))
	if err != nil {
		t.Fatalf("Ingest returned error: %v", err)
	}
	if item.Expression != "取り組む" || item.Reading != "とりくむ" || item.Meaning != "to tackle, to work on" {
		t.Fatalf("item = %+v, want expression/reading/meaning set", item)
	}
	if item.Source != "novel: コンビニ人間" {
		t.Fatalf("item.Source = %q, want %q", item.Source, "novel: コンビニ人間")
	}
	if item.Lookups != 1 {
		t.Fatalf("item.Lookups = %d, want 1", item.Lookups)
	}
	if item.ID == "" {
		t.Fatal("item.ID was not assigned")
	}

	if len(h.events.events) != 1 {
		t.Fatalf("recorded %d events, want 1: %+v", len(h.events.events), h.events.events)
	}
	ev := h.events.events[0]
	if ev.Type != event.TypeVocabularyLookedUp {
		t.Fatalf("event.Type = %q, want %q", ev.Type, event.TypeVocabularyLookedUp)
	}
	if ev.Subject != "取り組む" {
		t.Fatalf("event.Subject = %q, want 取り組む", ev.Subject)
	}
	if ev.IdentityID != testIdentity {
		t.Fatalf("event.IdentityID = %q, want %q", ev.IdentityID, testIdentity)
	}
}

// TestIngestSecondLookupIncrementsCount: a second, distinct-client-event
// lookup of the same expression increments Lookups rather than
// creating a second item.
func TestIngestSecondLookupIncrementsCount(t *testing.T) {
	h := newHarness()
	if _, err := h.svc.Ingest(context.Background(), testIdentity, lookupEvent("取り組む")); err != nil {
		t.Fatal(err)
	}
	item, err := h.svc.Ingest(context.Background(), testIdentity, lookupEvent("取り組む"))
	if err != nil {
		t.Fatal(err)
	}
	if item.Lookups != 2 {
		t.Fatalf("item.Lookups = %d, want 2", item.Lookups)
	}
	if len(h.events.events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(h.events.events))
	}
}

// TestIngestDuplicateClientEventIDIsIdempotent pins the brief's
// idempotency contract: a retried POST carrying the SAME
// client_event_id must not double-count Lookups or record a second
// learning event.
func TestIngestDuplicateClientEventIDIsIdempotent(t *testing.T) {
	h := newHarness()
	ev := lookupEvent("取り組む")
	ev.ClientEventID = "client-evt-1"

	first, err := h.svc.Ingest(context.Background(), testIdentity, ev)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.svc.Ingest(context.Background(), testIdentity, ev)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second.ID = %q, want same as first %q", second.ID, first.ID)
	}
	if second.Lookups != 1 {
		t.Fatalf("second.Lookups = %d, want 1 (no double-count on retry)", second.Lookups)
	}
	if len(h.events.events) != 1 {
		t.Fatalf("recorded %d events, want 1 (no duplicate event on retry)", len(h.events.events))
	}
}

// TestIngestUnsupportedTypeReturnsError: PRD §12 only supports
// "vocabulary.lookup" today — anything else is a 400-shaped error,
// nothing persisted or recorded.
func TestIngestUnsupportedTypeReturnsError(t *testing.T) {
	h := newHarness()
	ev := lookupEvent("取り組む")
	ev.Type = "vocabulary.something-else"

	_, err := h.svc.Ingest(context.Background(), testIdentity, ev)
	if !errors.Is(err, appvocabulary.ErrUnsupportedType) {
		t.Fatalf("err = %v, want ErrUnsupportedType", err)
	}
	if len(h.repo.items) != 0 {
		t.Fatalf("persisted %d items, want 0", len(h.repo.items))
	}
	if len(h.events.events) != 0 {
		t.Fatalf("recorded %d events, want 0", len(h.events.events))
	}
}

// seedItem ingests expression once (real path, through the service) so
// DetectProduction has something in AllExpressions to match against.
func (h *harness) seedItem(t *testing.T, expression string) {
	t.Helper()
	if _, err := h.svc.Ingest(context.Background(), testIdentity, lookupEvent(expression)); err != nil {
		t.Fatalf("seed lookup for %q failed: %v", expression, err)
	}
}

// TestDetectProductionMatchWithoutOverlapProducesCorrectly: the text
// contains a looked-up expression and no correction's Original
// overlaps its occurrence → RecordProduction(successful=true) and a
// vocabulary.produced-correctly event.
func TestDetectProductionMatchWithoutOverlapProducesCorrectly(t *testing.T) {
	h := newHarness()
	h.seedItem(t, "取り組む")
	baseline := len(h.events.events)

	err := h.svc.DetectProduction(context.Background(), testIdentity, testSessionID, "新しい仕事に取り組む。", nil)
	if err != nil {
		t.Fatalf("DetectProduction returned error: %v", err)
	}

	if len(h.repo.productions) != 1 {
		t.Fatalf("RecordProduction called %d times, want 1: %+v", len(h.repo.productions), h.repo.productions)
	}
	if !h.repo.productions[0].Successful {
		t.Fatalf("production call = %+v, want Successful=true", h.repo.productions[0])
	}

	newEvents := h.events.events[baseline:]
	if len(newEvents) != 1 {
		t.Fatalf("recorded %d new events, want 1: %+v", len(newEvents), newEvents)
	}
	ev := newEvents[0]
	if ev.Type != event.TypeVocabularyProducedCorrectly {
		t.Fatalf("event.Type = %q, want %q", ev.Type, event.TypeVocabularyProducedCorrectly)
	}
	if ev.Subject != "取り組む" {
		t.Fatalf("event.Subject = %q, want 取り組む", ev.Subject)
	}
	if ev.SessionID == nil || *ev.SessionID != testSessionID {
		t.Fatalf("event.SessionID = %v, want %q", ev.SessionID, testSessionID)
	}
}

// TestDetectProductionMatchWithOverlapProducesButNotCorrectly: a
// correction's Original overlaps the expression's occurrence →
// RecordProduction(successful=false) and a plain vocabulary.produced
// event (not produced-correctly).
func TestDetectProductionMatchWithOverlapProducesButNotCorrectly(t *testing.T) {
	h := newHarness()
	h.seedItem(t, "取り組む")

	err := h.svc.DetectProduction(context.Background(), testIdentity, testSessionID, "新しい仕事に取り組む。", []string{"仕事に取り組む"})
	if err != nil {
		t.Fatalf("DetectProduction returned error: %v", err)
	}

	if len(h.repo.productions) != 1 {
		t.Fatalf("RecordProduction called %d times, want 1", len(h.repo.productions))
	}
	if h.repo.productions[0].Successful {
		t.Fatalf("production call = %+v, want Successful=false (correction overlapped)", h.repo.productions[0])
	}

	var found bool
	for _, ev := range h.events.events {
		if ev.Type == event.TypeVocabularyProduced {
			found = true
		}
		if ev.Type == event.TypeVocabularyProducedCorrectly {
			t.Fatalf("recorded produced-correctly despite an overlapping correction: %+v", ev)
		}
	}
	if !found {
		t.Fatal("expected a vocabulary.produced event, found none")
	}
}

// TestDetectProductionIgnoresShortExpressions: a single-rune
// "expression" (e.g. a stray kana entry) must never be scanned as a
// substring — it would match nearly every sentence and produce pure
// noise.
func TestDetectProductionIgnoresShortExpressions(t *testing.T) {
	h := newHarness()
	h.seedItem(t, "に") // 1 rune

	err := h.svc.DetectProduction(context.Background(), testIdentity, testSessionID, "新しい仕事に取り組みます。", nil)
	if err != nil {
		t.Fatalf("DetectProduction returned error: %v", err)
	}
	if len(h.repo.productions) != 0 {
		t.Fatalf("RecordProduction called %d times, want 0 for a 1-rune expression", len(h.repo.productions))
	}
}

// TestDetectProductionNoMatchDoesNothing: an expression absent from
// the text records nothing.
func TestDetectProductionNoMatchDoesNothing(t *testing.T) {
	h := newHarness()
	h.seedItem(t, "取り組む")

	err := h.svc.DetectProduction(context.Background(), testIdentity, testSessionID, "今日はいい天気です。", nil)
	if err != nil {
		t.Fatalf("DetectProduction returned error: %v", err)
	}
	if len(h.repo.productions) != 0 {
		t.Fatalf("RecordProduction called %d times, want 0", len(h.repo.productions))
	}
}
