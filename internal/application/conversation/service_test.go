package conversation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	agentconversation "github.com/mikeyaustin/jlp/internal/agent/conversation"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
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

// fakeConversationRepo mirrors the real postgres repo's identity-scoped
// contract in memory: a conversation belongs to whichever identity
// created it, and InsertTurn/ListTurns both refuse any other identity
// with storage.ErrNotFound.
type fakeConversationRepo struct {
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
	row, ok := f.byConvID[conversationID]
	if !ok || row.identity != identity {
		return storage.ErrNotFound
	}
	f.turns[conversationID] = append(f.turns[conversationID], turn)
	return nil
}

func (f *fakeConversationRepo) ListTurns(_ context.Context, identity learner.IdentityID, conversationID string) ([]storage.ConversationTurn, error) {
	row, ok := f.byConvID[conversationID]
	if !ok || row.identity != identity {
		return nil, storage.ErrNotFound
	}
	return f.turns[conversationID], nil
}

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
func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used")
}
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used")
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
	svc := appconversation.NewService(repo, sessions, agent, vocabSvc, rec)
	return &harness{svc: svc, sessions: sessions, repo: repo, events: events, vocab: vocabRepo}
}

// --- Step 1 scenarios ---

func TestSayReturnsAReply(t *testing.T) {
	h := newHarness("immediate")
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "今日はいい天気ですね。")
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
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。")
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
	turn, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。")
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

	turn1, err := h.svc.Say(ctx, testIdentity, testSessionID, "昨日の映画はとても面白いでした。")
	if err != nil {
		t.Fatalf("Say turn 1 returned error: %v", err)
	}
	if len(turn1.Corrections) != 0 {
		t.Fatalf("turn 1 Corrections = %+v, want withheld (empty)", turn1.Corrections)
	}

	turn2, err := h.svc.Say(ctx, testIdentity, testSessionID, "今日は楽しいでした。")
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
	turn3, err := h.svc.Say(ctx, testIdentity, testSessionID, "映画について話しましょう。")
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

	_, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "それはそれとして、映画の話をしましょう。")
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
	_, err := h.svc.Say(context.Background(), "someone-else", testSessionID, "こんにちは。")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

func TestSummariseCrossIdentitySessionReturnsErrNotFound(t *testing.T) {
	h := newHarness("end")
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "こんにちは。"); err != nil {
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

	clean, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "今日はいい天気ですね。")
	if err != nil {
		t.Fatalf("Say (clean) returned error: %v", err)
	}
	if clean.Pending != 0 {
		t.Fatalf("clean message: Pending = %d, want 0", clean.Pending)
	}

	dirty, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。")
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
		if _, err := h.svc.Say(ctx, testIdentity, testSessionID, msg); err != nil {
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
	if _, err := h.svc.Say(context.Background(), testIdentity, testSessionID, "昨日の映画はとても面白いでした。"); err != nil {
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
