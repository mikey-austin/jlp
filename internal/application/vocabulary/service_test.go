package vocabulary_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	// deleted mirrors the real table's deleted_at column, keyed by item
	// id. Every read below honours it, because a fake that ignored soft
	// delete would let the Delete tests pass while the thing they are
	// actually asserting — "it stops coming back from reads" — went
	// untested.
	deleted map[string]time.Time
	nextID  int

	productions []productionCall
	bulkUpserts []bulkUpsertCall
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
		deleted:      map[string]time.Time{},
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
	// Resurrect-on-lookup, mirroring UpsertVocabularyItemOnLookup's
	// "deleted_at = NULL" in db/queries/vocabulary.sql.
	delete(f.deleted, item.ID)

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

// ListPage pages over List's own result the way the SQL does — sorted by
// (LastEvent DESC, ID DESC), strictly after the cursor, and reporting a
// next cursor only when another row actually follows.
//
// Implemented for real rather than stubbed: a fake that returned nothing
// would make every paging assertion pass against an empty list.
func (f *fakeVocabRepo) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
	all, err := f.List(ctx, identity, filter)
	if err != nil {
		return nil, storage.VocabularyCursor{}, err
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].LastEvent.Equal(all[j].LastEvent) {
			return all[i].LastEvent.After(all[j].LastEvent)
		}
		return all[i].ID > all[j].ID
	})

	var page []vocabulary.Item
	for _, item := range all {
		if !cursor.Zero() {
			after := item.LastEvent.Before(cursor.LastEvent) ||
				(item.LastEvent.Equal(cursor.LastEvent) && item.ID < cursor.ID)
			if !after {
				continue
			}
		}
		page = append(page, item)
		if len(page) > limit {
			break
		}
	}

	var next storage.VocabularyCursor
	if len(page) > limit {
		page = page[:limit]
		last := page[len(page)-1]
		next = storage.VocabularyCursor{LastEvent: last.LastEvent, ID: last.ID}
	}
	return page, next, nil
}

func (f *fakeVocabRepo) List(_ context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error) {
	var out []vocabulary.Item
	for _, item := range f.items {
		if item.IdentityID != identity {
			continue
		}
		if _, gone := f.deleted[item.ID]; gone {
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
		if item.IdentityID != identity {
			continue
		}
		if _, gone := f.deleted[item.ID]; gone {
			continue
		}
		out[item.Expression] = item.ID
	}
	return out, nil
}

// GetByExpressions mirrors the real adapter's bounded lookup (see
// storage.VocabularyRepository.GetByExpressions): every item, if any,
// matching (identity, one of expressions) via the same f.items map
// every other method here reads from.
func (f *fakeVocabRepo) GetByExpressions(_ context.Context, identity learner.IdentityID, expressions []string) ([]vocabulary.Item, error) {
	out := make([]vocabulary.Item, 0, len(expressions))
	for _, expr := range expressions {
		item, ok := f.items[vocabKey(identity, expr)]
		if !ok {
			continue
		}
		if _, gone := f.deleted[item.ID]; gone {
			continue
		}
		out = append(out, *item)
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

// bulkUpsertCall records one BulkUpsertWords invocation's arguments —
// TestIngestWords* asserts against this to prove IngestWords passes
// the whole (trimmed) batch through in one call, not one call per
// word.
type bulkUpsertCall struct {
	Identity learner.IdentityID
	Words    []storage.WordInput
}

// BulkUpsertWords mirrors the real adapter's sparse-merge contract
// (storage.VocabularyRepository.BulkUpsertWords' doc comment): empty
// incoming string fields never overwrite existing non-empty values,
// JLPTLevel 0 never overwrites a known level, and Tags replaces
// wholesale only when non-empty. Counters are never touched. Kind is
// always vocabulary.KindWord on first insert, matching the real
// adapter (this endpoint is word-only, unlike UpsertOnLookup's
// caller-supplied Kind).
// SoftDelete/Restore mirror the real adapter's contract exactly:
// identity-scoped, idempotent, and ErrNotFound for both "unknown id"
// and "someone else's id" — the property the cross-identity tests in
// softdelete_test.go rely on.
func (f *fakeVocabRepo) SoftDelete(_ context.Context, identity learner.IdentityID, itemID string, at time.Time) error {
	item, ok := f.byID[itemID]
	if !ok || item.IdentityID != identity {
		return storage.ErrNotFound
	}
	if _, already := f.deleted[itemID]; !already {
		f.deleted[itemID] = at
	}
	return nil
}

func (f *fakeVocabRepo) Restore(_ context.Context, identity learner.IdentityID, itemID string) error {
	item, ok := f.byID[itemID]
	if !ok || item.IdentityID != identity {
		return storage.ErrNotFound
	}
	delete(f.deleted, itemID)
	return nil
}

// isDeleted / exists let a test assert on the raw storage state rather
// than on what a read returns — the difference between "the delete was
// refused" and "the row was quietly removed anyway".
func (f *fakeVocabRepo) isDeleted(itemID string) bool {
	_, gone := f.deleted[itemID]
	return gone
}

func (f *fakeVocabRepo) exists(itemID string) bool {
	_, ok := f.byID[itemID]
	return ok
}

func (f *fakeVocabRepo) BulkUpsertWords(_ context.Context, identity learner.IdentityID, words []storage.WordInput, at time.Time) (int, error) {
	f.bulkUpserts = append(f.bulkUpserts, bulkUpsertCall{Identity: identity, Words: words})
	for _, w := range words {
		k := vocabKey(identity, w.Expression)
		item, ok := f.items[k]
		if !ok {
			f.nextID++
			item = &vocabulary.Item{
				ID:         fmt.Sprintf("vocab-%d", f.nextID),
				IdentityID: identity,
				Expression: w.Expression,
				Kind:       vocabulary.KindWord,
				FirstSeen:  at,
			}
			f.items[k] = item
			f.byID[item.ID] = item
		}
		item.LastEvent = at
		if w.Reading != "" {
			item.Reading = w.Reading
		}
		if w.Meaning != "" {
			item.Meaning = w.Meaning
		}
		if w.MeaningEN != "" {
			item.MeaningEN = w.MeaningEN
		}
		if w.JLPTLevel != 0 {
			item.JLPTLevel = w.JLPTLevel
		}
		if w.Source != "" {
			item.Source = w.Source
		}
		if len(w.Tags) > 0 {
			item.Tags = w.Tags
		}
	}
	return len(words), nil
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

// --- IngestWords (Phase 3 Task 8: POST /api/v1/words) ---

func benkyouWord() storage.WordInput {
	return storage.WordInput{
		Expression: "勉強",
		Reading:    "べんきょう",
		Meaning:    "学ぶこと、学習すること",
		MeaningEN:  "studying",
		JLPTLevel:  3,
		Tags:       []string{"education", "noun"},
		Source:     "Anki Deck",
	}
}

// TestIngestWordsHappyPathReturnsCountAndRecordsOneImportedEvent pins
// the brief's core contract: a valid 3-word batch returns imported=3
// and records EXACTLY ONE vocabulary.imported event (never one per
// word), carrying count and a sample of the imported expressions.
func TestIngestWordsHappyPathReturnsCountAndRecordsOneImportedEvent(t *testing.T) {
	h := newHarness()
	words := []storage.WordInput{
		benkyouWord(),
		{Expression: "猫", Reading: "ねこ", Meaning: "cat animal", Source: "Anki Deck"},
		{Expression: "犬", Reading: "いぬ", Meaning: "dog animal"},
	}

	count, err := h.svc.IngestWords(context.Background(), testIdentity, words)
	if err != nil {
		t.Fatalf("IngestWords returned error: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if len(h.repo.bulkUpserts) != 1 {
		t.Fatalf("BulkUpsertWords called %d times, want 1 (whole batch, not per-word)", len(h.repo.bulkUpserts))
	}
	if len(h.repo.bulkUpserts[0].Words) != 3 {
		t.Fatalf("BulkUpsertWords received %d words, want 3", len(h.repo.bulkUpserts[0].Words))
	}

	var imported []event.LearningEvent
	for _, ev := range h.events.events {
		if ev.Type == event.TypeVocabularyImported {
			imported = append(imported, ev)
		}
	}
	if len(imported) != 1 {
		t.Fatalf("recorded %d vocabulary.imported events, want exactly 1: %+v", len(imported), imported)
	}
	ev := imported[0]
	if ev.Subject != "Anki Deck" {
		t.Errorf("Subject = %q, want %q (first word's source)", ev.Subject, "Anki Deck")
	}
	if got := ev.Evidence["count"]; got != 3 {
		t.Errorf("Evidence[count] = %v, want 3", got)
	}
	sample, ok := ev.Evidence["sample"].([]string)
	if !ok || len(sample) != 3 || sample[0] != "勉強" {
		t.Errorf("Evidence[sample] = %#v, want [勉強 猫 犬]", ev.Evidence["sample"])
	}
}

// TestIngestWordsSubjectDefaultsToExternalWhenNoSource: when no word
// in the batch carries a Source, the event's Subject falls back to
// "external" rather than being left blank.
func TestIngestWordsSubjectDefaultsToExternalWhenNoSource(t *testing.T) {
	h := newHarness()
	words := []storage.WordInput{{Expression: "猫", Reading: "ねこ", Meaning: "cat"}}

	if _, err := h.svc.IngestWords(context.Background(), testIdentity, words); err != nil {
		t.Fatalf("IngestWords returned error: %v", err)
	}
	if len(h.events.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(h.events.events))
	}
	if got := h.events.events[0].Subject; got != "external" {
		t.Errorf("Subject = %q, want %q", got, "external")
	}
}

// TestIngestWordsSampleCapsAtFive: a batch larger than 5 words still
// carries only the first 5 expressions in Evidence["sample"] — the
// event log must not blow up for a 1000-word sync.
func TestIngestWordsSampleCapsAtFive(t *testing.T) {
	h := newHarness()
	words := make([]storage.WordInput, 0, 7)
	for i := 0; i < 7; i++ {
		words = append(words, storage.WordInput{
			Expression: fmt.Sprintf("word%d", i),
			Reading:    "reading",
			Meaning:    "meaning",
		})
	}

	count, err := h.svc.IngestWords(context.Background(), testIdentity, words)
	if err != nil {
		t.Fatalf("IngestWords returned error: %v", err)
	}
	if count != 7 {
		t.Fatalf("count = %d, want 7", count)
	}
	sample, _ := h.events.events[0].Evidence["sample"].([]string)
	if len(sample) != 5 {
		t.Fatalf("sample length = %d, want 5 (capped, even though the batch had 7)", len(sample))
	}
}

// TestIngestWordsMissingRequiredFieldNamesTheIndex covers each of
// kanji/reading/meaning being blank (including whitespace-only, which
// TrimSpace must catch): the returned error must be a
// *WordValidationError naming the exact 0-based index of the bad
// entry, and NOTHING must be written (BulkUpsertWords never called) —
// a batch either fully succeeds or fully fails.
func TestIngestWordsMissingRequiredFieldNamesTheIndex(t *testing.T) {
	cases := []struct {
		name  string
		words []storage.WordInput
	}{
		{"missing kanji", []storage.WordInput{
			benkyouWord(),
			{Expression: "  ", Reading: "ねこ", Meaning: "cat"},
		}},
		{"missing reading", []storage.WordInput{
			benkyouWord(),
			{Expression: "猫", Reading: "", Meaning: "cat"},
		}},
		{"missing meaning", []storage.WordInput{
			benkyouWord(),
			{Expression: "猫", Reading: "ねこ", Meaning: ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			_, err := h.svc.IngestWords(context.Background(), testIdentity, tc.words)
			var verr *appvocabulary.WordValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("err = %v, want a *WordValidationError", err)
			}
			if verr.Index != 1 {
				t.Errorf("Index = %d, want 1 (the second, invalid entry)", verr.Index)
			}
			if len(h.repo.bulkUpserts) != 0 {
				t.Error("BulkUpsertWords was called despite a validation failure — batch must be all-or-nothing")
			}
			if len(h.events.events) != 0 {
				t.Error("an event was recorded despite a validation failure")
			}
		})
	}
}

// TestIngestWordsInvalidJLPTLevelReturnsValidationError: jlpt_level
// outside 0..5 (the brief's example is 6) fails validation naming the
// offending index, same as a missing required field.
func TestIngestWordsInvalidJLPTLevelReturnsValidationError(t *testing.T) {
	h := newHarness()
	words := []storage.WordInput{
		benkyouWord(),
		{Expression: "猫", Reading: "ねこ", Meaning: "cat", JLPTLevel: 6},
	}

	_, err := h.svc.IngestWords(context.Background(), testIdentity, words)
	var verr *appvocabulary.WordValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v, want a *WordValidationError", err)
	}
	if verr.Index != 1 {
		t.Errorf("Index = %d, want 1", verr.Index)
	}
}

// TestIngestWordsTooManyWordsReturnsError: 1001 words (one over
// maxWordsPerRequest) is rejected without touching storage.
func TestIngestWordsTooManyWordsReturnsError(t *testing.T) {
	h := newHarness()
	words := make([]storage.WordInput, 1001)
	for i := range words {
		words[i] = storage.WordInput{Expression: fmt.Sprintf("w%d", i), Reading: "r", Meaning: "m"}
	}

	_, err := h.svc.IngestWords(context.Background(), testIdentity, words)
	if !errors.Is(err, appvocabulary.ErrTooManyWords) {
		t.Fatalf("err = %v, want ErrTooManyWords", err)
	}
	if err.Error() != "too many words in one request (max 1000)" {
		t.Errorf("err.Error() = %q, want the exact wire message", err.Error())
	}
	if len(h.repo.bulkUpserts) != 0 {
		t.Error("BulkUpsertWords was called despite an oversized batch")
	}
}

// TestIngestWordsEmptyBatchReturnsError: an empty "words" array is
// rejected — there's nothing to import.
func TestIngestWordsEmptyBatchReturnsError(t *testing.T) {
	h := newHarness()
	_, err := h.svc.IngestWords(context.Background(), testIdentity, nil)
	if !errors.Is(err, appvocabulary.ErrEmptyWordBatch) {
		t.Fatalf("err = %v, want ErrEmptyWordBatch", err)
	}
}
