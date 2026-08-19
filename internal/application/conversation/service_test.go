package conversation_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	agentconversation "github.com/mikeyaustin/jlp/internal/agent/conversation"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	testIdentity  learner.IdentityID = "learner-a"
	testSessionID session.ID         = "sess-1"
)

// --- in-memory fakes, identity-scoped like the real adapters (see
// application/feedback/service_test.go's established pattern this
// mirrors). ---

type fakeSessionRepo struct {
	byKey map[string]session.Session
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byKey: map[string]session.Session{}}
}

func sessKey(identity learner.IdentityID, id session.ID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeSessionRepo) Create(_ context.Context, s session.Session) error {
	f.byKey[sessKey(s.IdentityID, s.ID)] = s
	return nil
}

func (f *fakeSessionRepo) Get(_ context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	s, ok := f.byKey[sessKey(identity, id)]
	if !ok {
		return session.Session{}, storage.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessionRepo) List(_ context.Context, identity learner.IdentityID) ([]session.Session, error) {
	var out []session.Session
	for _, s := range f.byKey {
		if s.IdentityID == identity {
			out = append(out, s)
		}
	}
	return out, nil
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeSessionRepo) SoftDelete(context.Context, learner.IdentityID, session.ID, time.Time) error {
	return nil
}

func (f *fakeSessionRepo) Restore(context.Context, learner.IdentityID, session.ID) error {
	return nil
}

// fakeConversationRepo mirrors the real postgres repo's identity-scoped
// contract in memory: a conversation belongs to whichever identity
// created it, and InsertTurn/ListTurns both refuse any other identity
// with storage.ErrNotFound.
type fakeConversationRepo struct {
	// mu guards every field below — needed for
	// TestSayHandlesConcurrentDuplicateSpeechEventIDRace, the only test
	// that calls Say from more than one goroutine at once; a real
	// postgres.ConversationRepository gets this for free from the
	// database's own locking, but this in-memory fake needs it spelled
	// out or concurrent map writes panic for a reason that has nothing
	// to do with what that test is actually pinning.
	mu       sync.Mutex
	byConvID map[string]convRow
	// bySession keys conversation ID by identity+"/"+session, mirroring
	// the real schema's UNIQUE(session_id) — one conversation per
	// session regardless of who asks.
	bySession map[string]string
	turns     map[string][]storage.ConversationTurn // key: conversation ID
}

type convRow struct {
	identity learner.IdentityID
}

func newFakeConversationRepo() *fakeConversationRepo {
	return &fakeConversationRepo{byConvID: map[string]convRow{}, bySession: map[string]string{}, turns: map[string][]storage.ConversationTurn{}}
}

func (f *fakeConversationRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := string(sid)
	if id, ok := f.bySession[key]; ok {
		row := f.byConvID[id]
		if row.identity != identity {
			return "", false, storage.ErrNotFound
		}
		return id, false, nil
	}
	id := "conv-" + string(sid)
	f.byConvID[id] = convRow{identity: identity}
	f.bySession[key] = id
	return id, true, nil
}

func (f *fakeConversationRepo) InsertTurn(_ context.Context, identity learner.IdentityID, conversationID string, turn storage.ConversationTurn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.byConvID[conversationID]
	if !ok || row.identity != identity {
		return storage.ErrNotFound
	}
	f.turns[conversationID] = append(f.turns[conversationID], turn)
	return nil
}

func (f *fakeConversationRepo) ListTurns(_ context.Context, identity learner.IdentityID, conversationID string) ([]storage.ConversationTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.byConvID[conversationID]
	if !ok || row.identity != identity {
		return nil, storage.ErrNotFound
	}
	out := make([]storage.ConversationTurn, len(f.turns[conversationID]))
	copy(out, f.turns[conversationID])
	return out, nil
}

type fakeEventStore struct {
	mu     sync.Mutex
	events []event.LearningEvent
	// onAppendWithSpeechEventID, when set, is called BEFORE the
	// duplicate check for any Append whose evidence carries a
	// "speech_event_id" — used only by TestSayHandlesConcurrentDuplicate
	// SpeechEventIDRace, via raceBarrier below, to hold BOTH goroutines'
	// writes at the same point until both have arrived. Gating the
	// WRITE side (not the read side) is what makes this deterministic:
	// each goroutine only reaches this point after its OWN
	// validSourceEvent read has already completed, so by construction
	// neither can have observed the other's write yet (that write is
	// what's being held back) — reproducing the exact check-then-write
	// gap migrationsfs/00025_speech_event_id_unique.sql's partial index
	// closes, every run, rather than hoping Go's scheduler happens to
	// interleave two very fast goroutines that way on its own (it
	// usually won't — gating the READ side instead was tried first and
	// only reproduced the race ~1 run in 10, because the first
	// goroutine to resume typically runs Say to completion, INCLUDING
	// its own write, before the second goroutine's read even acquires
	// this store's mutex, which makes the second read correctly (but
	// uninterestingly) see "already attached" instead of racing at
	// Append — a false negative for what this test needs to pin).
	onAppendWithSpeechEventID func()
}

// raceBarrier returns a func that blocks until it has been called
// exactly n times, then releases every caller — a minimal cyclic
// barrier, used to line up N goroutines at the same point before
// letting any of them proceed past it.
func raceBarrier(n int) func() {
	var mu sync.Mutex
	count := 0
	release := make(chan struct{})
	return func() {
		mu.Lock()
		count++
		reached := count == n
		mu.Unlock()
		if reached {
			close(release)
		} else {
			<-release
		}
	}
}

// Append mirrors the REAL postgres repository's uniqueness enforcement
// for evidence["speech_event_id"] (migrationsfs/00025_speech_event_id_
// unique.sql's partial index, translated to storage.ErrDuplicate by
// postgres.LearningEventRepository.Append) — not just an unconditional
// append. This is what lets TestSayHandlesConcurrentDuplicateSpeech
// EventIDRace below genuinely exercise the check-then-write race two
// goroutines can hit: the mutex here plays the same "only one writer
// wins" role a real unique index does at the database level, so the
// SAME code path Say uses in production (catch storage.ErrDuplicate,
// downgrade, retry) is what's actually under test, not a fake that
// happens to never conflict.
func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	if _, ok := ev.Evidence["speech_event_id"]; ok && f.onAppendWithSpeechEventID != nil {
		f.onAppendWithSpeechEventID()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := ev.Evidence["speech_event_id"]; ok {
		for _, existing := range f.events {
			if existingID, ok2 := existing.Evidence["speech_event_id"]; ok2 && existingID == id {
				return storage.ErrDuplicate
			}
		}
	}
	f.events = append(f.events, ev)
	return nil
}

// ListRecent mirrors the REAL postgres repository's identity (and,
// when sid is non-nil, session) scoping exactly — not a passthrough of
// every event ever appended. This matters: Say's speech_event_id
// validation (code review Important, post-I1) relies on this scoping
// to reject another identity's or another session's genuine event id,
// and a fake that ignored scoping would let a forged-provenance test
// pass for the wrong reason (see TestSayIgnoresSpeechEventIDBelonging
// ToAnotherIdentity's own doc comment).
func (f *fakeEventStore) ListRecent(_ context.Context, identity learner.IdentityID, sid *session.ID, limit int) ([]event.LearningEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []event.LearningEvent
	for i := len(f.events) - 1; i >= 0; i-- { // newest first, like the real repo
		ev := f.events[i]
		if ev.IdentityID != identity {
			continue
		}
		if sid != nil && (ev.SessionID == nil || *ev.SessionID != *sid) {
			continue
		}
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (f *fakeEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event.LearningEvent, len(f.events))
	copy(out, f.events)
	return out, nil
}

// fakeVocabRepo is a minimal in-memory storage.VocabularyRepository
// double, wired into a real appvocabulary.Service so
// TestSayDetectsVocabularyProduction can observe DetectProduction's
// effect exactly like application/feedback's own equivalent test does.
type fakeVocabRepo struct {
	items       map[string]*vocabulary.Item
	productions []string // expression names RecordProduction was called for
}

func newFakeVocabRepo() *fakeVocabRepo { return &fakeVocabRepo{items: map[string]*vocabulary.Item{}} }

func (f *fakeVocabRepo) seed(id, expression string) {
	f.items[expression] = &vocabulary.Item{ID: id, Expression: expression}
}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used")
}
func (f *fakeVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabulary.Item, error) {
	panic("not used")
}
func (f *fakeVocabRepo) AllExpressions(_ context.Context, _ learner.IdentityID) (map[string]string, error) {
	out := map[string]string{}
	for expr, item := range f.items {
		out[expr] = item.ID
	}
	return out, nil
}
func (f *fakeVocabRepo) RecordProduction(_ context.Context, _ learner.IdentityID, itemID string, successful bool, _ time.Time) error {
	f.productions = append(f.productions, itemID)
	return nil
}
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	panic("not used")
}
func (f *fakeVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
	panic("not used by conversation service tests")
}

func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used")
}
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used")
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	return nil
}

// fakeGrammarRepo is a minimal in-memory storage.GrammarRepository:
// only ListConcepts is exercised by the conversation pipeline (Finding
// I-1 — it builds the conversation.turn prompt's candidate list and the
// known-slug set used to resolve/narrow each correction's tagged
// concepts, mirroring application/feedback.Service.RequestFeedback).
// Every other method panics if called.
type fakeGrammarRepo struct {
	concepts []grammar.Concept
}

func (f *fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by conversation service tests")
}
func (f *fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	return f.concepts, nil
}
func (f *fakeGrammarRepo) GetConcept(context.Context, string) (grammar.Concept, error) {
	panic("not used by conversation service tests")
}
func (f *fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by conversation service tests")
}
func (f *fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by conversation service tests")
}

// knownConceptsForFakeAI mirrors the two concept slugs
// internal/adapters/fakeai tags corrections with (i-adjective-past,
// particle-ni-direction) — see application/feedback/service_test.go's
// identically-named helper, which this is a local copy of (test
// packages don't import one another's unexported doubles).
func knownConceptsForFakeAI() []grammar.Concept {
	return []grammar.Concept{
		{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5},
		{Slug: "particle-ni-direction", Name: "に (direction/target/time)", JLPTLevel: 5},
	}
}

// spyGen records the last request it saw so a test can inspect the
// fully-rendered prompt — same pattern agent/conversation's own tests
// use.
type spyGen struct {
	req ai.StructuredRequest
}

func (s *spyGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	s.req = req
	return ai.StructuredResponse{JSON: []byte(`{"reply":"了解です。"}`), Provider: "spy", Model: "spy-1"}, nil
}

func testProfile(timing string) session.Profile {
	return session.Profile{
		TeacherMode:         "teacher",
		Strictness:          "balanced",
		ExplanationLanguage: "both",
		FeedbackTiming:      timing,
	}
}

type harness struct {
	svc      *appconversation.Service
	sessions *fakeSessionRepo
	repo     *fakeConversationRepo
	events   *fakeEventStore
	vocab    *fakeVocabRepo
}

func newHarness(timing string) *harness {
	sessions := newFakeSessionRepo()
	sessions.byKey[sessKey(testIdentity, testSessionID)] = session.Session{
		ID: testSessionID, IdentityID: testIdentity, Purpose: "Casual conversation practice", Profile: testProfile(timing),
	}
	repo := newFakeConversationRepo()
	events := &fakeEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	vocabRepo := newFakeVocabRepo()
	vocabSvc := appvocabulary.NewService(vocabRepo, rec)
	agent := agentconversation.New(fakeai.New())
	svc := appconversation.NewService(repo, sessions, agent, vocabSvc, rec, &fakeGrammarRepo{concepts: knownConceptsForFakeAI()}, events)
	return &harness{svc: svc, sessions: sessions, repo: repo, events: events, vocab: vocabRepo}
}

// --- Step 1 scenarios ---

func TestSayReturnsAReply(t *testing.T) {
	h := newHarness("immediate")
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "今日はいい天気ですね。", "")
	if err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	if turn.Reply == "" {
		t.Fatal("Turn.Reply is empty")
	}
	if turn.ID == "" {
		t.Fatal("Turn.ID was not assigned")
	}
}

func TestSayImmediateTimingShowsCorrectionsInline(t *testing.T) {
	h := newHarness("immediate")
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。", "")
	if err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	if len(turn.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(turn.Corrections), turn.Corrections)
	}
	if turn.Corrections[0].Replacement != "面白かったです" {
		t.Fatalf("Replacement = %q, want 面白かったです", turn.Corrections[0].Replacement)
	}
	if len(turn.Corrections[0].Concepts) != 1 || turn.Corrections[0].Concepts[0] != "i-adjective-past" {
		t.Fatalf("Concepts = %+v, want [i-adjective-past]", turn.Corrections[0].Concepts)
	}
	if turn.Timing != "immediate" {
		t.Fatalf("Timing = %q, want immediate", turn.Timing)
	}
}

// TestSayEndTimingWithholdsUntilSummarise pins the brief's mandate that
// "end" genuinely withholds: the correction DATA must be absent from
// the turn response (not merely hidden by CSS), and only Summarise
// releases it.
func TestSayEndTimingWithholdsUntilSummarise(t *testing.T) {
	h := newHarness("end")
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。", "")
	if err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	if len(turn.Corrections) != 0 {
		t.Fatalf("Say with end timing returned %d corrections in the turn response, want 0 (must be withheld, not just hidden): %+v", len(turn.Corrections), turn.Corrections)
	}

	summary, err := h.svc.Summarise(context.Background(), testIdentity, testSessionID)
	if err != nil {
		t.Fatalf("Summarise returned error: %v", err)
	}
	if summary.Turns != 1 {
		t.Fatalf("summary.Turns = %d, want 1", summary.Turns)
	}
	if len(summary.Corrections) != 1 {
		t.Fatalf("summary.Corrections = %d, want 1 (the digest must list the issue withheld during the conversation): %+v", len(summary.Corrections), summary.Corrections)
	}
	if summary.Corrections[0].Replacement != "面白かったです" {
		t.Fatalf("summary.Corrections[0].Replacement = %q, want 面白かったです", summary.Corrections[0].Replacement)
	}
}

// TestSayDelayedTimingBatchesAfterN pins delayed's own contract:
// individual turns before the batch boundary withhold, and the
// boundary turn releases everything accumulated since the last batch
// as one group.
func TestSayDelayedTimingBatchesAfterN(t *testing.T) {
	h := newHarness("delayed")
	ctx := context.Background()

	turn1, err := h.svc.Say(ctx, testIdentity, testSessionID, "昨日の映画はとても面白いでした。", "")
	if err != nil {
		t.Fatalf("Say turn 1 returned error: %v", err)
	}
	if len(turn1.Corrections) != 0 {
		t.Fatalf("turn 1 Corrections = %+v, want withheld (empty)", turn1.Corrections)
	}

	turn2, err := h.svc.Say(ctx, testIdentity, testSessionID, "今日は楽しいでした。", "")
	if err != nil {
		t.Fatalf("Say turn 2 returned error: %v", err)
	}
	if len(turn2.Corrections) != 0 {
		t.Fatalf("turn 2 Corrections = %+v, want withheld (empty)", turn2.Corrections)
	}

	// Turn 3 lands on the batch boundary (delayedBatchSize == 3): both
	// withheld corrections from turns 1-2 come back together with
	// turn 3's own (this message has no known mistake, so the batch is
	// exactly the two prior corrections).
	turn3, err := h.svc.Say(ctx, testIdentity, testSessionID, "映画について話しましょう。", "")
	if err != nil {
		t.Fatalf("Say turn 3 returned error: %v", err)
	}
	if len(turn3.Corrections) != 2 {
		t.Fatalf("turn 3 (batch boundary) Corrections = %d, want 2 (turns 1 and 2's withheld corrections): %+v", len(turn3.Corrections), turn3.Corrections)
	}
}

// TestSayDetectsVocabularyProduction pins the brief's "vocabulary
// production detected (reuse DetectProduction)" requirement: a
// conversation turn feeds the learner model exactly like a writing
// review does.
func TestSayDetectsVocabularyProduction(t *testing.T) {
	h := newHarness("immediate")
	h.vocab.seed("item-1", "それはそれとして")

	_, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "それはそれとして、映画の話をしましょう。", "")
	if err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	if len(h.vocab.productions) != 1 || h.vocab.productions[0] != "item-1" {
		t.Fatalf("vocab.productions = %+v, want [item-1]", h.vocab.productions)
	}

	foundProducedCorrectly := false
	for _, ev := range h.events.events {
		if ev.Type == event.TypeVocabularyProducedCorrectly {
			foundProducedCorrectly = true
		}
	}
	if !foundProducedCorrectly {
		t.Fatalf("no %s event recorded; events = %+v", event.TypeVocabularyProducedCorrectly, h.events.events)
	}
}

// TestSayCrossIdentitySessionReturnsErrNotFound pins the brief's
// mandated cross-identity test: a caller can never Say into a session
// belonging to another identity.
func TestSayCrossIdentitySessionReturnsErrNotFound(t *testing.T) {
	h := newHarness("immediate")
	_, err := h.svc.Say(context.Background(), "someone-else", testSessionID, "こんにちは。", "")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

func TestSummariseCrossIdentitySessionReturnsErrNotFound(t *testing.T) {
	h := newHarness("end")
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "こんにちは。", ""); err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	_, err := h.svc.Summarise(context.Background(), "someone-else", testSessionID)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestSayEndTimingCleanMessageIsNotFlaggedWithheld pins a code-review
// finding: a clean message under "end" (or "delayed" outside its batch
// boundary) timing must report Pending == 0, exactly like a message
// with an actual withheld correction must report Pending > 0 — the
// caller can't tell these apart from Corrections alone (both are
// empty), so it must not caption a clean turn as "corrections withheld"
// just because the session's timing policy generally withholds.
func TestSayEndTimingCleanMessageIsNotFlaggedWithheld(t *testing.T) {
	h := newHarness("end")

	clean, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "今日はいい天気ですね。", "")
	if err != nil {
		t.Fatalf("Say (clean) returned error: %v", err)
	}
	if clean.Pending != 0 {
		t.Fatalf("clean message: Pending = %d, want 0", clean.Pending)
	}

	dirty, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。", "")
	if err != nil {
		t.Fatalf("Say (dirty) returned error: %v", err)
	}
	if dirty.Pending != 1 {
		t.Fatalf("message with a withheld correction: Pending = %d, want 1", dirty.Pending)
	}
	if len(dirty.Corrections) != 0 {
		t.Fatalf("end timing: Corrections = %+v, want withheld (empty) even though Pending > 0", dirty.Corrections)
	}
}

// TestHistoryReDerivesPerTurnVisibilityForDelayedTiming pins History's
// contract: it must reproduce, per already-persisted turn, exactly what
// Say showed at the time — not simply return every turn's raw
// corrections unfiltered.
func TestHistoryReDerivesPerTurnVisibilityForDelayedTiming(t *testing.T) {
	h := newHarness("delayed")
	ctx := context.Background()
	for _, msg := range []string{"昨日の映画はとても面白いでした。", "今日は楽しいでした。", "映画について話しましょう。"} {
		if _, err := h.svc.Say(ctx, testIdentity, testSessionID, msg, ""); err != nil {
			t.Fatalf("Say returned error: %v", err)
		}
	}

	history, err := h.svc.History(ctx, testIdentity, testSessionID)
	if err != nil {
		t.Fatalf("History returned error: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("len(History) = %d, want 3", len(history))
	}
	if len(history[0].Corrections) != 0 || len(history[1].Corrections) != 0 {
		t.Fatalf("turns 1-2 Corrections = %+v / %+v, want both withheld", history[0].Corrections, history[1].Corrections)
	}
	if len(history[2].Corrections) != 2 {
		t.Fatalf("turn 3 (batch boundary) Corrections = %d, want 2", len(history[2].Corrections))
	}
}

// TestSayRecordsConversationTurnAndCorrectionEvents pins the event
// ordering/shape a conversation turn must record — mirroring
// application/feedback.Service.RequestFeedback's own
// feedback.requested/correction.presented event pair.
func TestSayRecordsConversationTurnAndCorrectionEvents(t *testing.T) {
	h := newHarness("immediate")
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。", ""); err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	if len(h.events.events) < 2 {
		t.Fatalf("recorded %d events, want at least 2 (conversation.turn + correction.presented): %+v", len(h.events.events), h.events.events)
	}
	if h.events.events[0].Type != event.TypeConversationTurn {
		t.Fatalf("events[0].Type = %q, want %q", h.events.events[0].Type, event.TypeConversationTurn)
	}
	if h.events.events[1].Type != event.TypeCorrectionPresented {
		t.Fatalf("events[1].Type = %q, want %q", h.events.events[1].Type, event.TypeCorrectionPresented)
	}
}

// seedEventCounter gives seedSpeechEvent's generated ids a little
// uniqueness across calls — a plain package-level counter is enough
// since these tests run sequentially, not in parallel.
var seedEventCounter int

// seedSpeechEvent appends a genuine speech.transcribed event straight
// into h's event store (bypassing Say/Recorder — this is test setup,
// not something under test) and returns its id, for tests that need a
// real, resolvable event id to validate sourceEventID against.
func seedSpeechEvent(t *testing.T, h *harness, identity learner.IdentityID, sid session.ID) string {
	t.Helper()
	seedEventCounter++
	id := fmt.Sprintf("speech-evt-%s-%s-%d", identity, sid, seedEventCounter)
	if err := h.events.Append(context.Background(), event.LearningEvent{
		ID:         id,
		IdentityID: identity,
		SessionID:  &sid,
		Type:       event.TypeSpeechTranscribed,
		Subject:    id,
		Evidence:   map[string]any{"duration_ms": 1000, "mime": "audio/wav", "chars": 3},
	}); err != nil {
		t.Fatalf("seedSpeechEvent: %v", err)
	}
	return id
}

// TestSayTagsTurnEventWithSourceSpeechEventID pins code review
// Important I1: a sourceEventID that resolves to a genuine
// speech.transcribed event — owned by identity, scoped to sid, not
// already used — lands verbatim on the resulting conversation.turn
// event's Evidence under "speech_event_id" — the join key that lets
// Task 9's learning-outcome analytics (or any future consumer) tell a
// spoken turn apart from a typed one. An empty sourceEventID (ordinary
// typed input, covered by TestSayRecordsConversationTurnAndCorrection
// Events above) must NOT add the key at all, not even as an empty
// string — a consumer checking "does this turn have a speech_event_id"
// must see a real map-key absence, not a falsy-but-present value.
func TestSayTagsTurnEventWithSourceSpeechEventID(t *testing.T) {
	h := newHarness("immediate")
	sourceID := seedSpeechEvent(t, h, testIdentity, testSessionID)
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。", sourceID); err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	var turnEvidence map[string]any
	for _, ev := range h.events.events {
		if ev.Type == event.TypeConversationTurn {
			turnEvidence = ev.Evidence
		}
	}
	if turnEvidence == nil {
		t.Fatal("no conversation.turn event recorded")
	}
	if turnEvidence["speech_event_id"] != sourceID {
		t.Fatalf("turn Evidence[speech_event_id] = %v, want %q", turnEvidence["speech_event_id"], sourceID)
	}

	// The typed-input case, in the SAME test for direct contrast: an
	// empty sourceEventID must leave the key entirely absent.
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "今日は楽しいでした。", ""); err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	var secondTurnEvidence map[string]any
	for _, ev := range h.events.events {
		if ev.Type == event.TypeConversationTurn && ev.Evidence["position"] == 2 {
			secondTurnEvidence = ev.Evidence
		}
	}
	if secondTurnEvidence == nil {
		t.Fatal("could not find the second conversation.turn event")
	}
	if _, present := secondTurnEvidence["speech_event_id"]; present {
		t.Fatalf("typed turn's Evidence unexpectedly has a speech_event_id key: %+v", secondTurnEvidence)
	}
}

// TestSayDowngradesUnresolvedSpeechEventIDToTyped pins the first of
// four provenance-forgery guards a follow-up code review required:
// a sourceEventID that doesn't resolve to ANY event at all (a client
// sending an arbitrary string) must silently downgrade the turn to
// typed — not error out and lose the learner's message, and not trust
// the unvalidated string either.
func TestSayDowngradesUnresolvedSpeechEventIDToTyped(t *testing.T) {
	h := newHarness("immediate")
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "映画について話しましょう。", "forged-does-not-exist")
	if err != nil {
		t.Fatalf("Say returned error: %v, want it to succeed with a downgraded (typed) turn", err)
	}
	if turn.Reply == "" {
		t.Fatal("the learner's message must still produce a turn, not be lost, on invalid provenance")
	}
	var turnEvidence map[string]any
	for _, ev := range h.events.events {
		if ev.Type == event.TypeConversationTurn {
			turnEvidence = ev.Evidence
		}
	}
	if _, present := turnEvidence["speech_event_id"]; present {
		t.Fatalf("Evidence unexpectedly carries speech_event_id for an unresolved id: %+v", turnEvidence)
	}
}

// TestSayIgnoresSpeechEventIDBelongingToAnotherIdentity is the case a
// naive existence-only check would let through: the id is genuine and
// really is a speech.transcribed event — just not this caller's. A
// malicious identity could otherwise forge provenance by reusing
// someone else's real event id.
func TestSayIgnoresSpeechEventIDBelongingToAnotherIdentity(t *testing.T) {
	h := newHarness("immediate")
	othersID := seedSpeechEvent(t, h, "someone-else", testSessionID)
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "映画について話しましょう。", othersID)
	if err != nil {
		t.Fatalf("Say returned error: %v, want a downgraded typed turn instead", err)
	}
	if turn.Reply == "" {
		t.Fatal("the learner's message must still produce a turn")
	}
	var turnEvidence map[string]any
	for _, ev := range h.events.events {
		if ev.Type == event.TypeConversationTurn && ev.IdentityID == testIdentity {
			turnEvidence = ev.Evidence
		}
	}
	if _, present := turnEvidence["speech_event_id"]; present {
		t.Fatalf("Evidence unexpectedly carries another identity's event id: %+v", turnEvidence)
	}
}

// TestSayIgnoresSpeechEventIDForDifferentSession pins the "for this
// session" condition: a real event, genuinely owned by identity, but
// recorded under a DIFFERENT session, must not be accepted either —
// scoping speech.transcribed events per-session is exactly what code
// review Important I1 introduced SessionID for in the first place.
func TestSayIgnoresSpeechEventIDForDifferentSession(t *testing.T) {
	h := newHarness("immediate")
	const otherSession session.ID = "sess-other"
	sourceID := seedSpeechEvent(t, h, testIdentity, otherSession)
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "映画について話しましょう。", sourceID)
	if err != nil {
		t.Fatalf("Say returned error: %v, want a downgraded typed turn instead", err)
	}
	if turn.Reply == "" {
		t.Fatal("the learner's message must still produce a turn")
	}
	var turnEvidence map[string]any
	for _, ev := range h.events.events {
		if ev.Type == event.TypeConversationTurn {
			turnEvidence = ev.Evidence
		}
	}
	if _, present := turnEvidence["speech_event_id"]; present {
		t.Fatalf("Evidence unexpectedly carries a different session's event id: %+v", turnEvidence)
	}
}

// TestSayIgnoresSpeechEventIDAlreadyAttachedToAnotherTurn pins the
// fourth condition: a real, correctly-owned, correctly-scoped event
// id that has ALREADY been consumed by an earlier turn must not be
// reusable for a second one — otherwise a client could tag every
// typed message in a session as speech-sourced by replaying one real
// event id.
func TestSayIgnoresSpeechEventIDAlreadyAttachedToAnotherTurn(t *testing.T) {
	h := newHarness("immediate")
	sourceID := seedSpeechEvent(t, h, testIdentity, testSessionID)
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。", sourceID); err != nil {
		t.Fatalf("first Say returned error: %v", err)
	}
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "今日は楽しいでした。", sourceID); err != nil {
		t.Fatalf("second Say returned error: %v", err)
	}
	var tagged int
	for _, ev := range h.events.events {
		if ev.Type == event.TypeConversationTurn {
			if id, ok := ev.Evidence["speech_event_id"]; ok && id == sourceID {
				tagged++
			}
		}
	}
	if tagged != 1 {
		t.Fatalf("turns tagged with the reused speech_event_id = %d, want exactly 1 (the first)", tagged)
	}
}

// TestSayHandlesConcurrentDuplicateSpeechEventIDRace is the genuine
// concurrency pin the fourth validSourceEvent condition needs — not
// just "the index exists", but that two ACTUAL goroutines racing the
// same real event id through Say both come out the other side
// correctly. Sequentially (TestSayIgnoresSpeechEventIDAlreadyAttached
// ToAnotherTurn above) the second call's own validSourceEvent read
// already sees the first call's write and downgrades before ever
// attempting to record — that never touches the retry-on-storage.
// ErrDuplicate path in service.go's Say at all. Two goroutines started
// together can both pass validSourceEvent's read BEFORE either write
// lands (the exact race migrationsfs/00025_speech_event_id_unique.sql
// closes at the database level — see fakeEventStore.Append's own doc
// comment for how this fake reproduces that same enforcement under a
// mutex), which is what actually exercises Say's storage.ErrDuplicate
// catch-and-retry.
func TestSayHandlesConcurrentDuplicateSpeechEventIDRace(t *testing.T) {
	h := newHarness("immediate")
	sourceID := seedSpeechEvent(t, h, testIdentity, testSessionID)
	// Hold both goroutines' writes at the same point until both have
	// arrived — see fakeEventStore.onAppendWithSpeechEventID's own doc
	// comment for why gating the write side (not the read side) is
	// what makes this deterministic rather than dependent on how Go's
	// scheduler happens to interleave two fast goroutines.
	h.events.onAppendWithSpeechEventID = raceBarrier(2)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	msgs := [2]string{"昨日の映画はとても面白いでした。", "今日は楽しいでした。"}
	wg.Add(2)
	for i := range 2 {
		go func(i int) {
			defer wg.Done()
			_, err := h.svc.Say(context.Background(), testIdentity, testSessionID, msgs[i], sourceID)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Say returned error %v — a lost race must downgrade silently, never fail the request", i, err)
		}
	}

	h.events.mu.Lock()
	all := append([]event.LearningEvent(nil), h.events.events...)
	h.events.mu.Unlock()

	var turns, tagged int
	for _, ev := range all {
		if ev.Type != event.TypeConversationTurn {
			continue
		}
		turns++
		if id, ok := ev.Evidence["speech_event_id"]; ok && id == sourceID {
			tagged++
		}
	}
	if turns != 2 {
		t.Fatalf("conversation.turn events = %d, want 2 — both messages must be recorded, race or not", turns)
	}
	if tagged != 1 {
		t.Fatalf("turns tagged with the raced speech_event_id = %d, want exactly 1 — the loser must downgrade to typed, not vanish or double-tag", tagged)
	}
}

// TestSayPassesConceptCandidatesToAgent pins Finding I-1: the brief
// requires conversation turns to feed the learner model exactly like
// writing does, including grammar concepts — which requires the agent
// actually be OFFERED a candidate list to tag from (see
// agent/conversation.TurnInput.ConceptCandidates and the
// conversation.turn.v1 system prompt's "ONLY from the provided
// candidate list" instruction). Using a spy generator instead of
// fakeai (which ignores candidates entirely) makes this a genuine pin
// on the SERVICE's wiring, not on fakeai's fixture behaviour: if
// Say never populates TurnInput.ConceptCandidates, the rendered user
// prompt never contains the "Known grammar concepts" section at all
// (conversation.turn.v1.user.md's {{if .ConceptCandidates}} guard).
func TestSayPassesConceptCandidatesToAgent(t *testing.T) {
	sessions := newFakeSessionRepo()
	sessions.byKey[sessKey(testIdentity, testSessionID)] = session.Session{
		ID: testSessionID, IdentityID: testIdentity, Purpose: "Casual conversation practice", Profile: testProfile("immediate"),
	}
	repo := newFakeConversationRepo()
	events := &fakeEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	gen := &spyGen{}
	agent := agentconversation.New(gen)
	svc := appconversation.NewService(repo, sessions, agent, nil, rec, &fakeGrammarRepo{concepts: knownConceptsForFakeAI()}, events)

	if _, err := svc.Say(context.Background(), testIdentity, testSessionID, "映画を見ました。", ""); err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	if !strings.Contains(gen.req.User, "Known grammar concepts") {
		t.Fatalf("rendered user prompt has no concept-candidates section — ConceptCandidates was never populated: %s", gen.req.User)
	}
	if !strings.Contains(gen.req.User, "i-adjective-past — い-adjective past tense") {
		t.Fatalf("rendered user prompt missing the known concept candidate line: %s", gen.req.User)
	}
}

// TestSayRecordsGrammarConceptEncounteredEvent pins the other half of
// Finding I-1: application/feedback.Service.RequestFeedback records one
// grammar.concept.encountered event per RESOLVED slug a correction was
// tagged with — application/conversation.Service must do the same, not
// stop at correction.presented/hint.shown.
func TestSayRecordsGrammarConceptEncounteredEvent(t *testing.T) {
	h := newHarness("immediate")
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。", ""); err != nil {
		t.Fatalf("Say returned error: %v", err)
	}
	found := false
	for _, ev := range h.events.events {
		if ev.Type == event.TypeGrammarConceptEncountered && ev.Subject == "i-adjective-past" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event recorded for i-adjective-past; events = %+v", event.TypeGrammarConceptEncountered, h.events.events)
	}
}

// TestSayDelayedTimingSecondBatchDoesNotReReleaseFirst pins the
// property the reviewer verified by hand but found untested (Finding
// I-6): after a first batch window releases (turns 1-3), a second
// window's boundary turn (turn 6) must release ONLY that window's own
// corrections (turns 4-6), never re-releasing turns 1-2's already-shown
// batch.
func TestSayDelayedTimingSecondBatchDoesNotReReleaseFirst(t *testing.T) {
	h := newHarness("delayed")
	ctx := context.Background()
	msgs := []string{
		"昨日の映画はとても面白いでした。", // turn 1: correction (withheld)
		"今日は楽しいでした。",       // turn 2: correction (withheld)
		"映画について話しましょう。",    // turn 3: batch boundary — releases turns 1-2 (2 corrections)
		"明日は晴れるでしょう。",      // turn 4: clean (withheld, new window)
		"猫が好きです。",          // turn 5: clean (withheld, new window)
		"それはとても面白いでした。",    // turn 6: batch boundary — must release ONLY turn 6's own (1)
	}
	var turns []appconversation.Turn
	for i, m := range msgs {
		turn, err := h.svc.Say(ctx, testIdentity, testSessionID, m, "")
		if err != nil {
			t.Fatalf("Say turn %d returned error: %v", i+1, err)
		}
		turns = append(turns, turn)
	}
	if len(turns[2].Corrections) != 2 {
		t.Fatalf("turn 3 (first batch boundary) Corrections = %d, want 2: %+v", len(turns[2].Corrections), turns[2].Corrections)
	}
	if len(turns[5].Corrections) != 1 {
		t.Fatalf("turn 6 (second batch boundary) Corrections = %d, want 1 (must NOT re-release turns 1-2's already-shown batch): %+v", len(turns[5].Corrections), turns[5].Corrections)
	}
}

// ListPage delegates to List and truncates. This package does not page;
// implemented rather than stubbed so it returns real rows if it ever
// starts. See storage.VocabularyRepository.ListPage.
func (f *fakeVocabRepo) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
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
func (r *fakeVocabRepo) ListRecentUnpracticed(context.Context, learner.IdentityID, time.Time, int) ([]vocabulary.Item, error) {
	panic("not used by these tests")
}

// GetByIDs resolves due words for 練習; the practice double below is the
// only one that needs real behaviour.
func (r *fakeVocabRepo) GetByIDs(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
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
