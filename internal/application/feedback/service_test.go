package feedback_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	appretrieval "github.com/mikeyaustin/jlp/internal/application/retrieval"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// --- in-memory fakes for the three repositories, identity-scoped like
// the real adapters (see application/writing/service_test.go for the
// established pattern this mirrors). ---

type fakeSessionRepo struct {
	sessions map[string]session.Session // key: identity+"/"+id
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{sessions: map[string]session.Session{}}
}

func sessKey(identity learner.IdentityID, id session.ID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeSessionRepo) Create(_ context.Context, s session.Session) error {
	f.sessions[sessKey(s.IdentityID, s.ID)] = s
	return nil
}

func (f *fakeSessionRepo) Get(_ context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	s, ok := f.sessions[sessKey(identity, id)]
	if !ok {
		return session.Session{}, storage.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessionRepo) List(_ context.Context, identity learner.IdentityID) ([]session.Session, error) {
	var out []session.Session
	for _, s := range f.sessions {
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

type fakeDocRepo struct {
	docs map[string]writing.Document // key: identity+"/"+id
}

func newFakeDocRepo() *fakeDocRepo {
	return &fakeDocRepo{docs: map[string]writing.Document{}}
}

func docKey(identity learner.IdentityID, id writing.DocumentID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeDocRepo) put(doc writing.Document) {
	f.docs[docKey(doc.IdentityID, doc.ID)] = doc
}

func (f *fakeDocRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, bool, error) {
	panic("not used by feedback service tests")
}

func (f *fakeDocRepo) Save(_ context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	panic("not used by feedback service tests")
}

func (f *fakeDocRepo) Get(_ context.Context, identity learner.IdentityID, id writing.DocumentID) (writing.Document, error) {
	doc, ok := f.docs[docKey(identity, id)]
	if !ok {
		return writing.Document{}, storage.ErrNotFound
	}
	return doc, nil
}

func (f *fakeDocRepo) ListVersions(_ context.Context, identity learner.IdentityID, id writing.DocumentID, limit int) ([]writing.Document, error) {
	panic("not used by feedback service tests")
}

// fakeFeedbackRepo mirrors the real postgres repo's identity check: a
// correction's identity is only known via the feedback record it
// belongs to, so UpdateCorrectionStatus joins through it exactly like
// the real "UPDATE ... FROM feedback_requests f WHERE ... f.identity_id
// = $2" query does.
type fakeFeedbackRepo struct {
	feedback    map[string]storage.FeedbackRecord   // key: feedback ID
	corrections map[string]storage.CorrectionRecord // key: correction ID
	concepts    []conceptRow                        // every persisted (correction, slug, resolved) tuple, flattened
	// insertErr fails InsertFeedback outright, before anything is
	// written — simulates the transaction never starting/committing.
	insertErr error
	// conceptInsertErr fails InsertFeedback ONLY when concepts is
	// non-empty, still writing nothing — simulates a same-transaction
	// rollback triggered specifically by the concept rows (a
	// correction_concepts constraint violation, say), proving the whole
	// InsertFeedback call is atomic and not "corrections committed,
	// concepts best-effort."
	conceptInsertErr error
}

func newFakeFeedbackRepo() *fakeFeedbackRepo {
	return &fakeFeedbackRepo{
		feedback:    map[string]storage.FeedbackRecord{},
		corrections: map[string]storage.CorrectionRecord{},
	}
}

// InsertFeedback mirrors the real repository's single-transaction
// contract (see storage.FeedbackRepository's doc comment): rec,
// corrections, and concepts are written together or none of them are.
func (f *fakeFeedbackRepo) InsertFeedback(_ context.Context, rec storage.FeedbackRecord, corrections []storage.CorrectionRecord, concepts map[string][]storage.ConceptTag) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	if f.conceptInsertErr != nil && len(concepts) > 0 {
		return f.conceptInsertErr
	}
	f.feedback[rec.ID] = rec
	for _, c := range corrections {
		f.corrections[c.ID] = c
	}
	for correctionID, tags := range concepts {
		for _, tag := range tags {
			f.concepts = append(f.concepts, conceptRow{CorrectionID: correctionID, Slug: tag.Slug, Resolved: tag.Resolved})
		}
	}
	return nil
}

func (f *fakeFeedbackRepo) UpdateCorrectionStatus(_ context.Context, identity learner.IdentityID, correctionID, status string) (storage.CorrectionRecord, error) {
	c, ok := f.corrections[correctionID]
	if !ok {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	fb, ok := f.feedback[c.FeedbackID]
	if !ok || fb.IdentityID != identity {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	c.Status = status
	c.SessionID = fb.SessionID
	f.corrections[correctionID] = c
	return c, nil
}

// identityScopedPresented mirrors the real repo's RetryCorrection/
// RevealCorrection WHERE clause: correctionID must exist, belong to
// identity (via the same feedback-record join UpdateCorrectionStatus
// uses), and still be Status "presented" — any other case is
// storage.ErrNotFound, collapsing "not yours"/"not found"/"not
// retriable" into one signal exactly like the real query does.
func (f *fakeFeedbackRepo) identityScopedPresented(identity learner.IdentityID, correctionID string) (storage.CorrectionRecord, session.ID, bool) {
	c, ok := f.corrections[correctionID]
	if !ok || c.Status != "presented" {
		return storage.CorrectionRecord{}, "", false
	}
	fb, ok := f.feedback[c.FeedbackID]
	if !ok || fb.IdentityID != identity {
		return storage.CorrectionRecord{}, "", false
	}
	return c, fb.SessionID, true
}

// RetryCorrection mirrors db/queries/feedback.sql's RetryCorrection:
// one atomic "increment attempts, accept iff trimmedAttempt ==
// Replacement" step.
func (f *fakeFeedbackRepo) RetryCorrection(_ context.Context, identity learner.IdentityID, correctionID, trimmedAttempt string) (storage.CorrectionRecord, error) {
	c, sessionID, ok := f.identityScopedPresented(identity, correctionID)
	if !ok {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	c.Attempts++
	if trimmedAttempt == c.Replacement {
		c.Status = "accepted"
	}
	c.SessionID = sessionID
	f.corrections[correctionID] = c
	return c, nil
}

// RevealCorrection mirrors db/queries/feedback.sql's RevealCorrection.
func (f *fakeFeedbackRepo) RevealCorrection(_ context.Context, identity learner.IdentityID, correctionID string) (storage.CorrectionRecord, error) {
	c, sessionID, ok := f.identityScopedPresented(identity, correctionID)
	if !ok {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	c.Revealed = true
	c.SessionID = sessionID
	f.corrections[correctionID] = c
	return c, nil
}

// RecordConfidence mirrors db/queries/feedback.sql's RecordConfidence:
// identity-scoped but NOT restricted to Status "presented" (a learner
// rates confidence AFTER a correction resolves).
func (f *fakeFeedbackRepo) RecordConfidence(_ context.Context, identity learner.IdentityID, correctionID string, confidence int) (storage.CorrectionRecord, error) {
	c, ok := f.corrections[correctionID]
	if !ok {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	fb, ok := f.feedback[c.FeedbackID]
	if !ok || fb.IdentityID != identity {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	v := confidence
	c.Confidence = &v
	c.SessionID = fb.SessionID
	f.corrections[correctionID] = c
	return c, nil
}

// GetCorrection mirrors the real query's identity-scoped join: a
// correction ID that exists but belongs to another identity misses
// with ErrNotFound exactly like UpdateCorrectionStatus.
func (f *fakeFeedbackRepo) GetCorrection(_ context.Context, identity learner.IdentityID, correctionID string) (storage.CorrectionRecord, error) {
	c, ok := f.corrections[correctionID]
	if !ok {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	fb, ok := f.feedback[c.FeedbackID]
	if !ok || fb.IdentityID != identity {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	c.SessionID = fb.SessionID
	return c, nil
}

func (f *fakeFeedbackRepo) RecentCorrections(context.Context, learner.IdentityID, int) ([]storage.CorrectionRecord, error) {
	panic("not used by feedback service tests")
}

// ListForSession mirrors the real query's identity+session scope
// (Phase 4 Task W item 2): a session that exists but belongs to
// another identity yields an empty slice, matching
// storage.FeedbackRepository's documented "list, not a lookup" miss
// shape.
func (f *fakeFeedbackRepo) ListForSession(_ context.Context, identity learner.IdentityID, sessionID session.ID) ([]storage.FeedbackSummary, error) {
	var out []storage.FeedbackSummary
	for _, fb := range f.feedback {
		if fb.IdentityID != identity || fb.SessionID != sessionID {
			continue
		}
		count := 0
		for _, c := range f.corrections {
			if c.FeedbackID == fb.ID {
				count++
			}
		}
		out = append(out, storage.FeedbackSummary{
			ID: fb.ID, SelectionText: fb.SelectionText, CorrectionCount: count, CreatedAt: fb.CreatedAt,
		})
	}
	return out, nil
}

// GetFeedback mirrors the real query's identity-scoped join, same
// "not yours"/"not found" collapse as UpdateCorrectionStatus.
func (f *fakeFeedbackRepo) GetFeedback(_ context.Context, identity learner.IdentityID, feedbackID string) (storage.FeedbackDetail, []storage.CorrectionRecord, error) {
	fb, ok := f.feedback[feedbackID]
	if !ok || fb.IdentityID != identity {
		return storage.FeedbackDetail{}, nil, storage.ErrNotFound
	}
	var corrections []storage.CorrectionRecord
	for _, c := range f.corrections {
		if c.FeedbackID == fb.ID {
			c.SessionID = fb.SessionID
			corrections = append(corrections, c)
		}
	}
	return storage.FeedbackDetail{
		ID: fb.ID, SelectionText: fb.SelectionText, CorrectedText: fb.CorrectedText,
	}, corrections, nil
}

// conceptRow is one persisted (correction, slug, resolved) tuple,
// letting tests assert exactly what InsertFeedback's concepts argument
// contained.
type conceptRow struct {
	CorrectionID string
	Slug         string
	Resolved     bool
}

// GetCorrectionConcepts mirrors the real query's "resolved only,
// slug-ascending" contract over f.concepts.
func (f *fakeFeedbackRepo) GetCorrectionConcepts(_ context.Context, correctionID string) ([]string, error) {
	var out []string
	for _, cc := range f.concepts {
		if cc.CorrectionID == correctionID && cc.Resolved {
			out = append(out, cc.Slug)
		}
	}
	sort.Strings(out)
	return out, nil
}

// fakeEventStore is an in-memory storage.LearningEventRepository, same
// role as application/writing/service_test.go's double: it lets tests
// assert exactly which events were recorded, in what order, without a
// database.
type fakeEventStore struct {
	events    []event.LearningEvent
	appendErr error
}

func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return f.events, nil
}

func (f *fakeEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	return f.events, nil
}

// fakeGrammarRepo is an in-memory storage.GrammarRepository: only
// ListConcepts is exercised by the feedback pipeline (it builds the
// teacher.feedback.v3 prompt's candidate list and the known-slug set
// used to classify each tag as resolved/unresolved), so every other
// method panics if called — a test that needs it should say so
// explicitly rather than silently getting a zero value.
type fakeGrammarRepo struct {
	concepts []grammar.Concept
}

func (f *fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by feedback service tests")
}

func (f *fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	return f.concepts, nil
}

func (f *fakeGrammarRepo) GetConcept(context.Context, string) (grammar.Concept, error) {
	panic("not used by feedback service tests")
}

func (f *fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by feedback service tests")
}

func (f *fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by feedback service tests")
}

// fakeObsRepo is a minimal in-memory storage.ObservationRepository
// double, needed only because planner.NewPlanner (wired into the
// harness below so feedback.Service can call its
// ActivationCandidates) requires one — no feedback service test
// exercises observations, so every method panics if actually called.
type fakeObsRepo struct{}

func (f *fakeObsRepo) Upsert(context.Context, learnermodel.Observation) error {
	panic("not used by feedback service tests")
}

func (f *fakeObsRepo) List(context.Context, learner.IdentityID) ([]learnermodel.Observation, error) {
	panic("not used by feedback service tests")
}

func (f *fakeObsRepo) DeleteAll(context.Context, learner.IdentityID) error {
	panic("not used by feedback service tests")
}

// fakePriorityRepo is an in-memory storage.PriorityRepository double:
// a test sets rows directly (via the top field) rather than needing a
// real planner.Recompute to populate them — these tests are about the
// feedback pipeline consuming Top(5), not about scoring itself (see
// internal/application/planner's own tests for that).
type fakePriorityRepo struct {
	top []storage.Priority
}

func (f *fakePriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by feedback service tests")
}

func (f *fakePriorityRepo) Top(_ context.Context, _ learner.IdentityID, limit int) ([]storage.Priority, error) {
	out := f.top
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// fakeVocabRepo is an in-memory storage.VocabularyRepository double
// wired into the harness's real appvocabulary.Service, letting
// TestRequestFeedbackDetectsVocabularyProduction (below) capture
// exactly what DetectProduction, called from the very end of
// RequestFeedback, records — without a database.
type fakeVocabRepo struct {
	items       map[string]*vocabulary.Item // key: expression (single test identity)
	productions []productionCall
	// recordErr, when set, fails every RecordProduction call — used by
	// TestRequestFeedbackVocabularyDetectionFailureIsNonFatal to prove
	// RequestFeedback swallows a downstream vocabulary failure rather
	// than propagating it to the caller.
	recordErr error
	// activateItems is what ListActivationCandidates sorts/caps and
	// returns — pre-set directly by a test (the same "canned data, not
	// a real filter re-implementation" style fakePriorityRepo.Top
	// already uses), since the real WHERE-clause filter logic is pinned
	// at the postgres integration layer (see
	// adapters/postgres/vocabulary_test.go), not re-derived here.
	// Consumed via planner.ActivationCandidates, wired into the
	// harness's real *planner.Planner (see newTestHarnessWithGenerator),
	// to fill teacher.ReviewInput.ExpressionsToEncourage.
	activateItems []vocabulary.Item
	// listErr, when set, fails every List call — used to prove a
	// planner.ActivationCandidates failure surfaces as a
	// RequestFeedback error.
	listErr error
	// allItems backs GetByExpressions — feedback.Service.
	// dueExpressionItems' resolve-a-due-subject-back-to-Reading/Meaning
	// step (PRD §54). Unset (nil) is fine for every test that never
	// seeds a due expression: dueExpressionItems only calls
	// GetByExpressions at all once retrieval.Scheduler.DueSubjects has
	// reported a due "expression" item, which nothing does by default.
	allItems []vocabulary.Item
}

type productionCall struct {
	ItemID     string
	Successful bool
}

func newFakeVocabRepo() *fakeVocabRepo {
	return &fakeVocabRepo{items: map[string]*vocabulary.Item{}}
}

// seed pre-populates an item as if it had already been looked up —
// tests use this instead of going through Ingest, since RequestFeedback
// only ever calls AllExpressions/RecordProduction, never UpsertOnLookup.
func (f *fakeVocabRepo) seed(id, expression string) {
	f.items[expression] = &vocabulary.Item{ID: id, Expression: expression}
}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used by feedback service tests")
}

func (f *fakeVocabRepo) RecordProduction(_ context.Context, _ learner.IdentityID, itemID string, successful bool, _ time.Time) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.productions = append(f.productions, productionCall{ItemID: itemID, Successful: successful})
	return nil
}

func (f *fakeVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabulary.Item, error) {
	panic("not used by feedback service tests")
}

// GetByExpressions is a bounded lookup over f.allItems (see that
// field's doc comment) — the real adapter's indexed WHERE expression =
// ANY(...) query, mirrored here as a plain linear filter since
// allItems is always small in these tests.
func (f *fakeVocabRepo) GetByExpressions(_ context.Context, _ learner.IdentityID, expressions []string) ([]vocabulary.Item, error) {
	want := make(map[string]bool, len(expressions))
	for _, e := range expressions {
		want[e] = true
	}
	out := make([]vocabulary.Item, 0, len(expressions))
	for _, it := range f.allItems {
		if want[it.Expression] {
			out = append(out, it)
		}
	}
	return out, nil
}

// ListActivationCandidates mirrors the real adapter's contract (sort
// activateItems by Lookups DESC, cap at limit) — the same "canned data,
// real sort/limit" shape application/planner's own fakeVocabRepo uses,
// since planner.ActivationCandidates is now a thin passthrough with no
// sort/limit logic of its own (see that method's doc comment).
func (f *fakeVocabRepo) ListActivationCandidates(_ context.Context, _ learner.IdentityID, limit int) ([]vocabulary.Item, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	items := append([]vocabulary.Item(nil), f.activateItems...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Lookups > items[j].Lookups })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	out := map[string]string{}
	for expr, item := range f.items {
		out[expr] = item.ID
	}
	return out, nil
}

// SeedBank is not used by feedback service tests — the harness's
// planner.Planner only ever calls ListActivationCandidates, never the
// seed path.
func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used by feedback service tests")
}

// BulkUpsertWords is not used by feedback service tests either — same
// reasoning as SeedBank above.
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used by feedback service tests")
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeVocabRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	return nil
}

func (f *fakeVocabRepo) Restore(context.Context, learner.IdentityID, string) error {
	return nil
}

// fakeRetrievalRepo is a storage.RetrievalRepository double: only Due
// is exercised by feedback.Service.dueExpressionItems (via
// retrieval.Scheduler.DueSubjects) — Upsert/Get/List all panic if
// called, the same "only what this package uses" convention every
// other fake in this file follows. due is set directly by a test that
// wants expressionsToEncourage to see a due expression.
type fakeRetrievalRepo struct {
	due []storage.RetrievalItem
}

func (f *fakeRetrievalRepo) Upsert(context.Context, storage.RetrievalItem) error {
	panic("not used by feedback service tests")
}
func (f *fakeRetrievalRepo) Get(context.Context, learner.IdentityID, string, string) (storage.RetrievalItem, error) {
	panic("not used by feedback service tests")
}

// Due truncates to limit (mirroring the real postgres adapter's
// LIMIT): a fake that ignored limit would make dueExpressionsScanLimit's
// actual value untestable, since dueExpressionItems never re-truncates
// what Due returns.
func (f *fakeRetrievalRepo) Due(_ context.Context, _ learner.IdentityID, _ time.Time, limit int) ([]storage.RetrievalItem, error) {
	out := f.due
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeRetrievalRepo) List(context.Context, learner.IdentityID, int) ([]storage.RetrievalItem, error) {
	panic("not used by feedback service tests")
}

// knownConceptsForFakeAI mirrors the two concept slugs
// internal/adapters/fakeai tags corrections with (i-adjective-past,
// particle-ni-direction), so the default test harness's candidate list
// matches what fakeai's deterministic rules actually produce.
func knownConceptsForFakeAI() []grammar.Concept {
	return []grammar.Concept{
		{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5},
		{Slug: "particle-ni-direction", Name: "に (direction/target/time)", JLPTLevel: 5},
	}
}

const testIdentity = learner.IdentityID("learner-a")
const testSessionID = session.ID("sess-1")
const testDocID = writing.DocumentID("doc-1")

// testHarness wires a real feedback.Service over in-memory repos, a
// real teacher.Agent over fakeai (deterministic, no network), and a
// real learning.Recorder over a fake event store + a real in-process
// bus — the same "real collaborators, fake edges" shape
// application/writing/service_test.go uses.
type testHarness struct {
	svc        *appfeedback.Service
	sessions   *fakeSessionRepo
	docs       *fakeDocRepo
	repo       *fakeFeedbackRepo
	grammar    *fakeGrammarRepo
	events     *fakeEventStore
	priorities *fakePriorityRepo
	vocab      *fakeVocabRepo
	retrieval  *fakeRetrievalRepo
}

// newTestHarness wires the default harness over fakeai — its
// GrammarRepository double knows exactly the two concept slugs fakeai's
// deterministic rules tag (see knownConceptsForFakeAI), so a review of
// a known-bad fakeai sentence resolves its tag(s) the same way the real
// pipeline would. newTestHarnessWithGenerator lets a test swap in a
// different ai.StructuredGenerator (e.g. to return an unknown concept
// slug) while keeping everything else the same.
func newTestHarness() *testHarness {
	return newTestHarnessWithGenerator(fakeai.New())
}

func newTestHarnessWithGenerator(gen ai.StructuredGenerator) *testHarness {
	sessions := newFakeSessionRepo()
	docs := newFakeDocRepo()
	repo := newFakeFeedbackRepo()
	grammarRepo := &fakeGrammarRepo{concepts: knownConceptsForFakeAI()}
	priorities := &fakePriorityRepo{}
	events := &fakeEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	vocabRepo := newFakeVocabRepo()
	vocabSvc := appvocabulary.NewService(vocabRepo, rec)
	// teachingPlanner is a REAL planner.Planner (not a fake) over
	// vocabRepo, the same "real collaborator, fake edges" shape the rest
	// of this harness uses — RequestFeedback calls its
	// ActivationCandidates directly (see this package's controller
	// resolution in the Task 7 brief: a thin repo passthrough, not
	// Recompute, so it's safe to leave on the request path). obs/events/
	// grammar/priorities are only there to satisfy NewPlanner's
	// signature; ActivationCandidates never touches them.
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, events, grammarRepo, priorities, vocabRepo, time.Now)
	t := teacher.New(gen)
	// retrievalRepo starts with nothing due — TestExpressionsToEncourage*
	// tests that care about the due-preference path set retrievalRepo.due
	// directly (via the returned harness's retrieval field) before
	// calling RequestFeedback.
	retrievalRepo := &fakeRetrievalRepo{}
	retrievalSched := appretrieval.NewScheduler(retrievalRepo, time.Now)
	svc := appfeedback.NewService(sessions, docs, repo, grammarRepo, priorities, teachingPlanner, vocabSvc, t, rec, false, nil, retrievalSched)
	return &testHarness{svc: svc, sessions: sessions, docs: docs, repo: repo, grammar: grammarRepo, events: events, priorities: priorities, vocab: vocabRepo, retrieval: retrievalRepo}
}

func (h *testHarness) putSession(s session.Session) {
	h.sessions.sessions[sessKey(s.IdentityID, s.ID)] = s
}

func testProfile() session.Profile {
	return session.Profile{
		TeacherMode:         "teacher",
		Strictness:          "balanced",
		ExplanationLanguage: "both",
	}
}

// (a) full happy path on the PRD sentence: feedback persisted, 1
// correction "presented", events recorded in order feedback.requested,
// correction.presented.
func TestRequestFeedbackHappyPath(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})
	runeLen := len([]rune(content))

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity:   testIdentity,
		SessionID:  testSessionID,
		DocumentID: testDocID,
		Start:      0,
		End:        runeLen,
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	if len(fb.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(fb.Corrections), fb.Corrections)
	}
	cv := fb.Corrections[0]
	if cv.Replacement != "面白かったです" {
		t.Fatalf("Replacement = %q, want 面白かったです", cv.Replacement)
	}
	if cv.Status != "presented" {
		t.Fatalf("Status = %q, want presented", cv.Status)
	}
	if len(cv.Diff) == 0 {
		t.Fatal("per-correction Diff was not computed")
	}
	if fb.Corrected != "とても面白かったです" {
		t.Fatalf("Corrected = %q, want とても面白かったです", fb.Corrected)
	}
	if len(fb.Diff) == 0 {
		t.Fatal("whole-selection Diff was not computed")
	}
	if fb.ID == "" {
		t.Fatal("Feedback ID was not assigned")
	}

	// Persisted: one FeedbackRecord, one CorrectionRecord at Position 0,
	// Status "presented", CorrectedText set.
	if len(h.repo.feedback) != 1 {
		t.Fatalf("persisted feedback count = %d, want 1", len(h.repo.feedback))
	}
	rec := h.repo.feedback[fb.ID]
	if rec.CorrectedText != "とても面白かったです" {
		t.Fatalf("persisted CorrectedText = %q, want とても面白かったです", rec.CorrectedText)
	}
	if rec.SelectionStart != 0 || rec.SelectionEnd != runeLen {
		t.Fatalf("persisted selection bounds = [%d,%d], want [0,%d]", rec.SelectionStart, rec.SelectionEnd, runeLen)
	}
	if len(h.repo.corrections) != 1 {
		t.Fatalf("persisted corrections count = %d, want 1", len(h.repo.corrections))
	}
	for _, c := range h.repo.corrections {
		if c.Position != 0 {
			t.Fatalf("persisted correction Position = %d, want 0", c.Position)
		}
		if c.Status != "presented" {
			t.Fatalf("persisted correction Status = %q, want presented", c.Status)
		}
		if c.FeedbackID != fb.ID {
			t.Fatalf("persisted correction FeedbackID = %q, want %q", c.FeedbackID, fb.ID)
		}
	}

	// Events recorded in order: feedback.requested, correction.presented,
	// then grammar.concept.encountered (面白いでした→面白かったです is
	// fakeai's i-adjective-past rule, and the harness's GrammarRepository
	// double knows that slug, so the tag resolves and gets an event).
	if len(h.events.events) != 3 {
		t.Fatalf("recorded %d events, want 3: %+v", len(h.events.events), h.events.events)
	}
	first := h.events.events[0]
	if first.Type != event.TypeFeedbackRequested {
		t.Fatalf("events[0].Type = %q, want %q", first.Type, event.TypeFeedbackRequested)
	}
	if first.Subject != string(testDocID) {
		t.Fatalf("events[0].Subject = %q, want %q", first.Subject, testDocID)
	}
	if got := first.Evidence["start"]; got != 0 {
		t.Fatalf("events[0].Evidence[start] = %v, want 0", got)
	}
	if got := first.Evidence["end"]; got != runeLen {
		t.Fatalf("events[0].Evidence[end] = %v, want %d", got, runeLen)
	}
	if got := first.Evidence["corrections"]; got != 1 {
		t.Fatalf("events[0].Evidence[corrections] = %v, want 1", got)
	}
	second := h.events.events[1]
	if second.Type != event.TypeCorrectionPresented {
		t.Fatalf("events[1].Type = %q, want %q", second.Type, event.TypeCorrectionPresented)
	}
	if second.Subject != cv.ID {
		t.Fatalf("events[1].Subject = %q, want correction ID %q", second.Subject, cv.ID)
	}
	if got := second.Evidence["type"]; got != string(cv.Type) {
		t.Fatalf("events[1].Evidence[type] = %v, want %q", got, cv.Type)
	}
	if got := second.Evidence["severity"]; got != string(cv.Severity) {
		t.Fatalf("events[1].Evidence[severity] = %v, want %q", got, cv.Severity)
	}
	third := h.events.events[2]
	if third.Type != event.TypeGrammarConceptEncountered {
		t.Fatalf("events[2].Type = %q, want %q", third.Type, event.TypeGrammarConceptEncountered)
	}
	if third.Subject != "i-adjective-past" {
		t.Fatalf("events[2].Subject = %q, want i-adjective-past", third.Subject)
	}
	if third.SessionID == nil || *third.SessionID != testSessionID {
		t.Fatalf("events[2].SessionID = %v, want %q", third.SessionID, testSessionID)
	}
	if got := third.Evidence["correction_id"]; got != cv.ID {
		t.Fatalf("events[2].Evidence[correction_id] = %v, want %q", got, cv.ID)
	}
	if got := third.Evidence["type"]; got != string(cv.Type) {
		t.Fatalf("events[2].Evidence[type] = %v, want %q", got, cv.Type)
	}
	if got := third.Evidence["severity"]; got != string(cv.Severity) {
		t.Fatalf("events[2].Evidence[severity] = %v, want %q", got, cv.Severity)
	}

	// correction_concepts: one resolved row for i-adjective-past.
	if len(h.repo.concepts) != 1 {
		t.Fatalf("persisted concept rows = %d, want 1: %+v", len(h.repo.concepts), h.repo.concepts)
	}
	cc := h.repo.concepts[0]
	if cc.CorrectionID != cv.ID || cc.Slug != "i-adjective-past" || !cc.Resolved {
		t.Fatalf("persisted concept row = %+v, want {CorrectionID:%q Slug:i-adjective-past Resolved:true}", cc, cv.ID)
	}
}

// TestRequestFeedbackUnknownConceptSlugPersistsUnresolvedWithoutEvent
// pins the other half of Step 2: a teacher response tagging a
// correction with a slug that ISN'T in the GrammarRepository catalog
// still gets a correction_concepts row (resolved=false) — tagging bugs
// or a stale candidate list must be visible in the data, not silently
// dropped — but must NOT produce a grammar.concept.encountered event,
// since the learner model can't explain a concept it has no catalog
// entry for.
func TestRequestFeedbackUnknownConceptSlugPersistsUnresolvedWithoutEvent(t *testing.T) {
	gen := &stubConceptGen{
		original:    "行きました",
		replacement: "行った",
		concepts:    []string{"totally-unknown-slug"},
	}
	h := newTestHarnessWithGenerator(gen)
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "昨日、公園に行きました。"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if len(fb.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(fb.Corrections), fb.Corrections)
	}
	correctionID := fb.Corrections[0].ID

	if len(h.repo.concepts) != 1 {
		t.Fatalf("persisted concept rows = %d, want 1: %+v", len(h.repo.concepts), h.repo.concepts)
	}
	cc := h.repo.concepts[0]
	if cc.CorrectionID != correctionID || cc.Slug != "totally-unknown-slug" || cc.Resolved {
		t.Fatalf("persisted concept row = %+v, want {CorrectionID:%q Slug:totally-unknown-slug Resolved:false}", cc, correctionID)
	}

	for _, ev := range h.events.events {
		if ev.Type == event.TypeGrammarConceptEncountered {
			t.Fatalf("recorded a %s event for an unresolved slug, want none: %+v", event.TypeGrammarConceptEncountered, ev)
		}
	}
}

// TestRequestFeedbackDuplicateConceptSlugsDedupedToOneRowAndOneEvent
// pins the code-review fix: a teacher response that (redundantly) tags
// the SAME correction with the same slug twice must still persist
// exactly one correction_concepts row and record exactly one
// grammar.concept.encountered event for it — not two. Task 4's
// weakness counts key off the event stream per concept, so an
// undeduped double-tag would silently inflate them.
func TestRequestFeedbackDuplicateConceptSlugsDedupedToOneRowAndOneEvent(t *testing.T) {
	gen := &stubConceptGen{
		original:    "行きました",
		replacement: "行った",
		concepts:    []string{"i-adjective-past", "i-adjective-past"},
	}
	h := newTestHarnessWithGenerator(gen)
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "昨日、公園に行きました。"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if len(fb.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(fb.Corrections), fb.Corrections)
	}
	correctionID := fb.Corrections[0].ID

	// Exactly one correction_concepts row for the slug, not two.
	if len(h.repo.concepts) != 1 {
		t.Fatalf("persisted concept rows = %d, want 1 (deduped): %+v", len(h.repo.concepts), h.repo.concepts)
	}
	cc := h.repo.concepts[0]
	if cc.CorrectionID != correctionID || cc.Slug != "i-adjective-past" || !cc.Resolved {
		t.Fatalf("persisted concept row = %+v, want {CorrectionID:%q Slug:i-adjective-past Resolved:true}", cc, correctionID)
	}

	// Exactly one grammar.concept.encountered event, not two.
	count := 0
	for _, ev := range h.events.events {
		if ev.Type == event.TypeGrammarConceptEncountered {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recorded %d grammar.concept.encountered events, want 1 (deduped): %+v", count, h.events.events)
	}
}

// TestRequestFeedbackInitialViewCarriesOnlyDedupedResolvedConcepts pins
// the fix-round-3 Finding 1: the initial CorrectionView.Concepts (built
// straight off the AI result, before this fix) could disagree with the
// view SetCorrectionStatus later rebuilds from GetCorrectionConcepts —
// the initial one showed raw, untriaged slugs (unresolved ones that
// 404 at /grammar/{slug}, and pre-dedup duplicates), the later one
// showed deduped/resolved-only ones. resolveConceptTags is now the
// single source of truth for both: a teacher response tagging one
// correction with an unresolved slug AND a resolved slug tagged twice
// must produce an initial view carrying exactly the deduped, resolved
// slug — nothing else.
func TestRequestFeedbackInitialViewCarriesOnlyDedupedResolvedConcepts(t *testing.T) {
	gen := &stubConceptGen{
		original:    "行きました",
		replacement: "行った",
		concepts:    []string{"totally-unknown-slug", "i-adjective-past", "i-adjective-past"},
	}
	h := newTestHarnessWithGenerator(gen)
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "昨日、公園に行きました。"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if len(fb.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(fb.Corrections), fb.Corrections)
	}
	cv := fb.Corrections[0]

	// The initial view: exactly the deduped, resolved slug — never the
	// unresolved one, never a duplicate.
	if len(cv.Concepts) != 1 || cv.Concepts[0] != "i-adjective-past" {
		t.Fatalf("initial view Concepts = %v, want exactly [i-adjective-past]", cv.Concepts)
	}

	// Both slugs are still persisted (nothing silently dropped from the
	// data) — one resolved, one not, and the duplicate collapsed to one
	// row per slug.
	if len(h.repo.concepts) != 2 {
		t.Fatalf("persisted concept rows = %d, want 2: %+v", len(h.repo.concepts), h.repo.concepts)
	}
	got := map[string]bool{}
	for _, cc := range h.repo.concepts {
		if cc.CorrectionID != cv.ID {
			t.Fatalf("persisted concept row for wrong correction: %+v, want CorrectionID %q", cc, cv.ID)
		}
		got[cc.Slug] = cc.Resolved
	}
	if resolved, ok := got["i-adjective-past"]; !ok || !resolved {
		t.Fatalf("i-adjective-past row: present=%v resolved=%v, want present resolved=true", ok, resolved)
	}
	if resolved, ok := got["totally-unknown-slug"]; !ok || resolved {
		t.Fatalf("totally-unknown-slug row: present=%v resolved=%v, want present resolved=false", ok, resolved)
	}

	// Exactly one grammar.concept.encountered event — for the resolved
	// slug only, and only once despite the duplicate tag.
	count := 0
	for _, ev := range h.events.events {
		if ev.Type == event.TypeGrammarConceptEncountered {
			count++
			if ev.Subject != "i-adjective-past" {
				t.Fatalf("grammar.concept.encountered Subject = %q, want i-adjective-past", ev.Subject)
			}
		}
	}
	if count != 1 {
		t.Fatalf("recorded %d grammar.concept.encountered events, want 1", count)
	}
}

// TestRequestFeedbackConceptPersistFailureLeavesNothingCommitted pins
// fix-round-3 Finding 2: concept tags used to persist in a SEPARATE
// transaction, run per-correction AFTER InsertFeedback (and its
// feedback.requested event) had already committed — so a failure
// specifically in the concepts write left feedback_requests/corrections
// durably committed while the client saw a 500 and (typically) retried
// under freshly-generated IDs, double-counting concept encounters
// downstream. Now concepts are part of InsertFeedback's single
// transaction: a failure anywhere in that call — simulated here via
// conceptInsertErr, which only fires when concepts is non-empty, the
// same shape a real correction_concepts constraint violation deep in
// the same tx would take — must leave NOTHING committed: no feedback
// record, no corrections, no concept rows, and (since RequestFeedback
// returns before ever calling Record) no events either.
func TestRequestFeedbackConceptPersistFailureLeavesNothingCommitted(t *testing.T) {
	h := newTestHarness() // fakeai on とても面白いでした tags i-adjective-past, so concepts is non-empty
	h.repo.conceptInsertErr = errors.New("boom: correction_concepts constraint violation")
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err == nil {
		t.Fatal("expected an error when concept persistence fails, got nil")
	}
	if len(h.repo.feedback) != 0 {
		t.Fatalf("persisted feedback count = %d, want 0 (whole insert must roll back)", len(h.repo.feedback))
	}
	if len(h.repo.corrections) != 0 {
		t.Fatalf("persisted corrections count = %d, want 0 (whole insert must roll back)", len(h.repo.corrections))
	}
	if len(h.repo.concepts) != 0 {
		t.Fatalf("persisted concept rows = %d, want 0", len(h.repo.concepts))
	}
	if len(h.events.events) != 0 {
		t.Fatalf("recorded %d events, want 0 (nothing should be recorded when persistence fails)", len(h.events.events))
	}
}

// TestSetCorrectionStatusAcceptedCarriesConceptChip pins the other
// code-review fix: correctionViewFromRecord's Correction has no
// Concepts (storage.CorrectionRecord carries none — tags live in
// correction_concepts), so SetCorrectionStatus must fetch them back
// via GetCorrectionConcepts and populate the returned CorrectionView,
// or the correction card's concept chip silently vanishes the moment a
// learner accepts/rejects it.
func TestSetCorrectionStatusAcceptedCarriesConceptChip(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	correctionID := fb.Corrections[0].ID
	if len(fb.Corrections[0].Concepts) == 0 {
		t.Fatalf("precondition failed: initial correction has no concepts to carry forward: %+v", fb.Corrections[0])
	}

	view, err := h.svc.SetCorrectionStatus(context.Background(), testIdentity, correctionID, "accepted")
	if err != nil {
		t.Fatalf("SetCorrectionStatus returned error: %v", err)
	}
	if view.Status != "accepted" {
		t.Fatalf("Status = %q, want accepted", view.Status)
	}
	if len(view.Concepts) != 1 || view.Concepts[0] != "i-adjective-past" {
		t.Fatalf("view.Concepts = %v, want [i-adjective-past]", view.Concepts)
	}
}

// capturingGen is a minimal ai.StructuredGenerator that records the
// last ai.StructuredRequest it was asked to generate — letting a test
// inspect the RENDERED prompt (System/User strings), not just the
// domain-level ReviewInput that produced it — and returns one fixed,
// schema-valid correction so RequestFeedback can complete normally.
type capturingGen struct {
	lastReq ai.StructuredRequest
}

func (c *capturingGen) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	c.lastReq = req
	payload := `{"corrections":[{"original":"面白いでした","replacement":"面白かったです","type":"conjugation","severity":"incorrect","explanation":{"ja":"テスト","en":"test"},"concepts":["i-adjective-past"]}]}`
	return ai.StructuredResponse{JSON: []byte(payload), Provider: "stub", Model: "stub-1"}, nil
}

// TestRequestFeedbackRecentErrorsReachRenderedPrompt is Phase 2 Task
// 5's closing-the-adapt-loop pin (PRD §16): a top priority seeded into
// the PriorityRepository must actually reach the rendered
// teacher.feedback.v3 USER prompt the AI generator receives — not just
// the in-memory ReviewInput.RecentErrors slice, which the v1 prompt
// bug could have silently dropped. Captured via a stub generator, per
// the brief's Step 3.
func TestRequestFeedbackRecentErrorsReachRenderedPrompt(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.priorities.top = []storage.Priority{
		{
			SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5,
			Reason: "recurring weakness: 5 occurrences in 30d (persistence 1.67), 2 in last 7d (recency 3), value 1.50 (N5 concept, weight 2)",
		},
	}
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	wantLine := "i-adjective-past (concept weakness, score 7.5): recurring weakness: 5 occurrences in 30d"
	if !strings.Contains(gen.lastReq.User, wantLine) {
		t.Fatalf("rendered prompt User = %q, missing the priority line %q", gen.lastReq.User, wantLine)
	}
	if gen.lastReq.PromptVersion != "v3" {
		t.Fatalf("PromptVersion = %q, want v3 (the version whose user template renders RecentErrors)", gen.lastReq.PromptVersion)
	}
}

// TestRequestFeedbackProviderOverrideReachesGeneratedRequest pins Phase
// 4 Task W item 5's plumbing: Request.ProviderOverride must reach the
// generated ai.StructuredRequest.ProviderOverride verbatim (via
// teacher.ReviewInput — see teacher.go's ReviewWriting) so airouter can
// actually honor it. It also pins item 1's "which provider/model
// actually served it" requirement: the returned Feedback.Provider/Model
// come straight from the generator's own response, not from the
// requested override.
func TestRequestFeedbackProviderOverrideReachesGeneratedRequest(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
		ProviderOverride: "agycli",
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if gen.lastReq.ProviderOverride != "agycli" {
		t.Fatalf("generated request ProviderOverride = %q, want agycli", gen.lastReq.ProviderOverride)
	}
	// capturingGen answers with Provider "stub"/Model "stub-1" regardless
	// of what was requested — Feedback.Provider/Model must reflect THAT
	// (what actually served it), not the "agycli" that was asked for.
	if fb.Provider != "stub" || fb.Model != "stub-1" {
		t.Fatalf("Feedback.Provider/Model = %q/%q, want stub/stub-1 (what the generator actually reported, not the requested override)", fb.Provider, fb.Model)
	}
}

// TestRequestFeedbackNoPrioritiesOmitsRecentErrorsSection: an identity
// with no priorities yet must render a prompt with no RecentErrors
// section at all (the v2 template's {{if .RecentErrors}} guard), not an
// empty-but-present one.
func TestRequestFeedbackNoPrioritiesOmitsRecentErrorsSection(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if strings.Contains(gen.lastReq.User, "recurring problem areas") {
		t.Fatalf("rendered prompt User unexpectedly contains the RecentErrors section header with no priorities seeded: %q", gen.lastReq.User)
	}
}

// stubConceptGen is a minimal ai.StructuredGenerator that always
// returns one correction tagging concepts (unvalidated, and may
// contain duplicates — see the dedupe test above) — used to exercise
// concept-tagging edge cases without needing fakeai to know about
// slugs that, by construction, aren't part of its two hardcoded rules.
type stubConceptGen struct {
	original, replacement string
	concepts              []string
}

func (s *stubConceptGen) GenerateStructured(_ context.Context, _ ai.StructuredRequest) (ai.StructuredResponse, error) {
	conceptsJSON, err := json.Marshal(s.concepts)
	if err != nil {
		return ai.StructuredResponse{}, err
	}
	payload := fmt.Sprintf(`{"corrections":[{"original":%q,"replacement":%q,"type":"conjugation","severity":"incorrect","explanation":{"ja":"テスト","en":"test"},"concepts":%s}]}`,
		s.original, s.replacement, conceptsJSON)
	return ai.StructuredResponse{JSON: []byte(payload), Provider: "stub", Model: "stub-1"}, nil
}

// (b) Start==End → whole document reviewed, regardless of where the
// cursor sits. A doc with a natural prefix plus the known-bad sentence,
// requested at a mid-document cursor with Start==End=5 (which by
// itself is an empty selection): only reviewing the whole document
// finds the correction.
func TestRequestFeedbackStartEqualsEndReviewsWholeDocument(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "これは前の文です。とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity:   testIdentity,
		SessionID:  testSessionID,
		DocumentID: testDocID,
		Start:      5,
		End:        5,
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if len(fb.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1 (whole document should have been reviewed): %+v", len(fb.Corrections), fb.Corrections)
	}
	rec := h.repo.feedback[fb.ID]
	if rec.SelectionStart != 0 || rec.SelectionEnd != len([]rune(content)) {
		t.Fatalf("persisted selection bounds = [%d,%d], want the whole document [0,%d]", rec.SelectionStart, rec.SelectionEnd, len([]rune(content)))
	}
	if rec.SelectionText != content {
		t.Fatalf("persisted SelectionText = %q, want the whole document %q", rec.SelectionText, content)
	}
}

// (c) cross-identity session → storage.ErrNotFound, nothing persisted.
func TestRequestFeedbackCrossIdentitySessionReturnsErrNotFound(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity:   "learner-mallory",
		SessionID:  testSessionID,
		DocumentID: testDocID,
		Start:      0,
		End:        len([]rune(content)),
	})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
	if len(h.repo.feedback) != 0 {
		t.Fatalf("persisted feedback count = %d, want 0 (nothing should persist on auth failure)", len(h.repo.feedback))
	}
	if len(h.events.events) != 0 {
		t.Fatalf("recorded %d events, want 0 (nothing should be recorded on auth failure)", len(h.events.events))
	}
}

// (d) SetCorrectionStatus("accepted") → status updated + a
// correction.accepted event.
func TestSetCorrectionStatusAccepted(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	correctionID := fb.Corrections[0].ID
	baseline := len(h.events.events)

	view, err := h.svc.SetCorrectionStatus(context.Background(), testIdentity, correctionID, "accepted")
	if err != nil {
		t.Fatalf("SetCorrectionStatus returned error: %v", err)
	}
	if view.Status != "accepted" {
		t.Fatalf("Status = %q, want accepted", view.Status)
	}
	if view.ID != correctionID {
		t.Fatalf("returned view ID = %q, want %q", view.ID, correctionID)
	}
	if h.repo.corrections[correctionID].Status != "accepted" {
		t.Fatalf("persisted correction Status = %q, want accepted", h.repo.corrections[correctionID].Status)
	}

	if len(h.events.events) != baseline+1 {
		t.Fatalf("recorded %d new events, want 1", len(h.events.events)-baseline)
	}
	last := h.events.events[len(h.events.events)-1]
	if last.Type != event.TypeCorrectionAccepted {
		t.Fatalf("event Type = %q, want %q", last.Type, event.TypeCorrectionAccepted)
	}
	if last.Subject != correctionID {
		t.Fatalf("event Subject = %q, want %q", last.Subject, correctionID)
	}
	// Regression: the correction.accepted event must carry the owning
	// session so the workspace's per-session activity feed (which
	// filters ListRecent by session ID) actually shows it — a nil
	// SessionID here would silently vanish from every session's
	// activity pane.
	if last.SessionID == nil || *last.SessionID != testSessionID {
		t.Fatalf("event SessionID = %v, want %q", last.SessionID, testSessionID)
	}
}

// (e) invalid status string → error, nothing changed.
func TestSetCorrectionStatusInvalidStatus(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	correctionID := fb.Corrections[0].ID
	baseline := len(h.events.events)

	_, err = h.svc.SetCorrectionStatus(context.Background(), testIdentity, correctionID, "bogus")
	if err == nil {
		t.Fatal("expected an error for an invalid status string, got nil")
	}
	if h.repo.corrections[correctionID].Status != "presented" {
		t.Fatalf("persisted correction Status = %q, want unchanged presented", h.repo.corrections[correctionID].Status)
	}
	if len(h.events.events) != baseline {
		t.Fatalf("recorded %d new events, want 0 for a rejected invalid status", len(h.events.events)-baseline)
	}
}

// TestRequestFeedbackDetectsVocabularyProduction pins the Task 6 hook:
// RequestFeedback, at the very end, calls vocab.DetectProduction over
// the reviewed text — a looked-up expression present in that text
// (here, untouched by the round's one correction) gets
// RecordProduction(successful=true) and a vocabulary.produced-correctly
// event, alongside the ordinary feedback.requested/correction.presented
// events the round already produces.
func TestRequestFeedbackDetectsVocabularyProduction(t *testing.T) {
	h := newTestHarness() // fakeai's i-adjective-past rule fires on 面白いでした
	h.vocab.seed("vocab-1", "取り組む")
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした。新しい仕事に取り組む。"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	if len(h.vocab.productions) != 1 {
		t.Fatalf("RecordProduction called %d times, want 1: %+v", len(h.vocab.productions), h.vocab.productions)
	}
	prod := h.vocab.productions[0]
	if prod.ItemID != "vocab-1" || !prod.Successful {
		t.Fatalf("production call = %+v, want {ItemID:vocab-1 Successful:true} (the correction never touched 取り組む)", prod)
	}

	var found bool
	for _, ev := range h.events.events {
		if ev.Type == event.TypeVocabularyProducedCorrectly {
			found = true
			if ev.Subject != "取り組む" {
				t.Fatalf("event.Subject = %q, want 取り組む", ev.Subject)
			}
			if ev.SessionID == nil || *ev.SessionID != testSessionID {
				t.Fatalf("event.SessionID = %v, want %q", ev.SessionID, testSessionID)
			}
		}
	}
	if !found {
		t.Fatal("expected a vocabulary.produced-correctly event, found none")
	}
}

// TestRequestFeedbackVocabularyDetectionFailureIsNonFatal pins the
// brief's explicit non-fatal contract: unlike every event.Record call
// in RequestFeedback (hard-fail), a vocab.DetectProduction error must
// never turn an otherwise-successful review into an error for the
// caller — production detection is enrichment, not core review
// history. Exercised by wiring a vocab service whose RecordProduction
// always errors.
func TestRequestFeedbackVocabularyDetectionFailureIsNonFatal(t *testing.T) {
	h := newTestHarness()
	h.vocab.seed("vocab-1", "取り組む")
	h.vocab.recordErr = errors.New("boom: vocab repo unavailable")
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "新しい仕事に取り組む。"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v, want nil (vocabulary detection failures must be non-fatal)", err)
	}
	// The review itself must have completed normally despite the
	// downstream vocabulary failure.
	if fb.ID == "" {
		t.Fatal("Feedback ID was not assigned")
	}
	if len(h.repo.feedback) != 1 {
		t.Fatalf("persisted feedback count = %d, want 1 (the review itself must still succeed)", len(h.repo.feedback))
	}
}

// TestRequestFeedbackExpressionsToEncourageReachRenderedPrompt is Task
// 7's closing-the-loop pin (PRD §55/§17.5): an activation candidate
// seeded into the VocabularyRepository (via planner.ActivationCandidates,
// a thin passthrough over List(filter="activate")) must actually reach
// the rendered teacher.feedback.v3 USER prompt, formatted
// "expression (reading) — meaning" — not just live in the in-memory
// ReviewInput.ExpressionsToEncourage slice a template bug could
// silently drop. Captured via a stub generator, mirroring
// TestRequestFeedbackRecentErrorsReachRenderedPrompt's Task 5 pin.
func TestRequestFeedbackExpressionsToEncourageReachRenderedPrompt(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.vocab.activateItems = []vocabulary.Item{
		{Expression: "それはそれとして", Reading: "それはそれとして", Meaning: "that aside; setting that aside", Kind: vocabulary.KindExpression},
		{Expression: "気がしないでもない", Reading: "きがしないでもない", Meaning: "I do feel a bit like...", Kind: vocabulary.KindExpression},
	}
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	if !strings.Contains(gen.lastReq.User, "gently encourage one") {
		t.Fatalf("rendered prompt User = %q, missing the encourage-block instruction", gen.lastReq.User)
	}
	// Reading equals Expression here (both pure kana), so the
	// formatted line must omit the redundant parenthetical.
	wantLine := "- それはそれとして — that aside; setting that aside"
	if !strings.Contains(gen.lastReq.User, wantLine) {
		t.Fatalf("rendered prompt User = %q, missing %q", gen.lastReq.User, wantLine)
	}
	if gen.lastReq.PromptVersion != "v3" {
		t.Fatalf("PromptVersion = %q, want v3", gen.lastReq.PromptVersion)
	}
}

// TestRequestFeedbackExpressionsToEncourageDueExpressionComesFirst pins
// the brief's "the feedback prompt's existing 'expressions to
// encourage' block prefers due expressions" (PRD §54): a due
// expression must appear in the encourage block AHEAD of a plain
// activation candidate, even though the activation candidate has far
// higher Lookups (which would otherwise put it first).
func TestRequestFeedbackExpressionsToEncourageDueExpressionComesFirst(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.vocab.activateItems = []vocabulary.Item{
		{Expression: "気がしないでもない", Reading: "きがしないでもない", Meaning: "I do feel a bit like...", Kind: vocabulary.KindExpression, Lookups: 99},
	}
	h.vocab.allItems = []vocabulary.Item{
		{Expression: "積もる", Reading: "つもる", Meaning: "to pile up", Kind: vocabulary.KindExpression},
	}
	h.retrieval.due = []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "積もる"},
	}
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	prompt := gen.lastReq.User
	dueLine := "積もる (つもる) — to pile up"
	activationLine := "気がしないでもない"
	dueIdx := strings.Index(prompt, dueLine)
	activationIdx := strings.Index(prompt, activationLine)
	if dueIdx == -1 {
		t.Fatalf("rendered prompt missing the due expression's line %q: %q", dueLine, prompt)
	}
	if activationIdx == -1 {
		t.Fatalf("rendered prompt missing the activation candidate's line: %q", prompt)
	}
	if dueIdx > activationIdx {
		t.Fatalf("due expression line (at %d) did not precede the activation candidate's (at %d): %q", dueIdx, activationIdx, prompt)
	}
}

// TestRequestFeedbackExpressionsToEncourageDueExpressionDeduplicated
// pins that a due expression which ALSO appears in the plain activation
// ranking is listed exactly once, not twice.
func TestRequestFeedbackExpressionsToEncourageDueExpressionDeduplicated(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.vocab.activateItems = []vocabulary.Item{
		{Expression: "積もる", Reading: "つもる", Meaning: "to pile up", Kind: vocabulary.KindExpression},
	}
	h.vocab.allItems = []vocabulary.Item{
		{Expression: "積もる", Reading: "つもる", Meaning: "to pile up", Kind: vocabulary.KindExpression},
	}
	h.retrieval.due = []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "積もる"},
	}
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	if count := strings.Count(gen.lastReq.User, "積もる"); count != 1 {
		t.Fatalf("rendered prompt mentions 積もる %d times, want exactly 1 (deduplicated): %q", count, gen.lastReq.User)
	}
}

// TestRequestFeedbackExpressionsToEncourageFindsDueExpressionBehindConcepts
// pins dueExpressionsScanLimit's actual value: the due queue's five
// most-due items are all concept-type (grammar due for review, PRD
// §54's OTHER due surface — see practice.Service.dueConcept), with the
// due expression only 6th. A scan capped at 5 (the pre-fix value) would
// see nothing but concepts and never notice the expression — this test
// fails under that reversion.
func TestRequestFeedbackExpressionsToEncourageFindsDueExpressionBehindConcepts(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.vocab.allItems = []vocabulary.Item{
		{Expression: "積もる", Reading: "つもる", Meaning: "to pile up", Kind: vocabulary.KindExpression},
	}
	h.retrieval.due = []storage.RetrievalItem{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "concept-1"},
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "concept-2"},
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "concept-3"},
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "concept-4"},
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "concept-5"},
		{IdentityID: testIdentity, SubjectType: "expression", Subject: "積もる"},
	}
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	if !strings.Contains(gen.lastReq.User, "積もる (つもる) — to pile up") {
		t.Fatalf("rendered prompt missing the due expression (6th in the queue, behind 5 due concepts): %q", gen.lastReq.User)
	}
}

// TestRequestFeedbackNoActivationCandidatesOmitsEncourageSection: an
// identity with no activation candidates yet must render a prompt with
// no ExpressionsToEncourage section at all (the v2 template's
// {{if .ExpressionsToEncourage}} guard), not an empty-but-present one —
// the default harness (h.vocab.activateItems unset) exercises this.
func TestRequestFeedbackNoActivationCandidatesOmitsEncourageSection(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if strings.Contains(gen.lastReq.User, "gently encourage") {
		t.Fatalf("rendered prompt User unexpectedly contains the encourage section with no activation candidates seeded: %q", gen.lastReq.User)
	}
}

// TestRequestFeedbackExpressionsToEncourageCappedAtFive pins the
// brief's "ActivationCandidates(5)" cap: seeding 7 candidates must
// still only render 5 lines, the highest-Lookups ones (planner.
// ActivationCandidates orders by Lookups DESC before limiting — see
// that method's own unit tests in application/planner for the sort
// itself; this test is about the cap actually reaching the prompt).
func TestRequestFeedbackExpressionsToEncourageCappedAtFive(t *testing.T) {
	gen := &capturingGen{}
	h := newTestHarnessWithGenerator(gen)
	items := make([]vocabulary.Item, 7)
	for i := range items {
		items[i] = vocabulary.Item{
			Expression: fmt.Sprintf("expr%d", i),
			Meaning:    "m",
			Kind:       vocabulary.KindExpression,
			Lookups:    7 - i, // expr0 highest, expr6 lowest
		}
	}
	h.vocab.activateItems = items
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	count := strings.Count(gen.lastReq.User, "- expr")
	if count != 5 {
		t.Fatalf("rendered prompt has %d encourage lines, want 5 (capped): %q", count, gen.lastReq.User)
	}
	for _, want := range []string{"expr0", "expr1", "expr2", "expr3", "expr4"} {
		if !strings.Contains(gen.lastReq.User, want) {
			t.Fatalf("rendered prompt missing top-5-by-lookups candidate %q: %q", want, gen.lastReq.User)
		}
	}
	for _, notWant := range []string{"expr5", "expr6"} {
		if strings.Contains(gen.lastReq.User, notWant) {
			t.Fatalf("rendered prompt unexpectedly contains lower-ranked candidate %q: %q", notWant, gen.lastReq.User)
		}
	}
}

// TestRequestFeedbackActivationCandidatesErrorPropagates: a failure
// listing activation candidates must surface as a RequestFeedback
// error, the same fatal treatment RecentErrors' underlying Top(5) call
// gets — this is a query feeding the prompt, not enrichment layered on
// top of an already-successful review (contrast with vocab.
// DetectProduction's deliberately non-fatal treatment, below).
func TestRequestFeedbackActivationCandidatesErrorPropagates(t *testing.T) {
	h := newTestHarness()
	h.vocab.listErr = errors.New("boom: vocab repo unavailable")
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err == nil {
		t.Fatal("expected an error when listing activation candidates fails, got nil")
	}
	if len(h.repo.feedback) != 0 {
		t.Fatalf("persisted feedback count = %d, want 0 (nothing should persist before the review even starts)", len(h.repo.feedback))
	}
}

// --- Phase 2 Task 8: active recall (hints, retry, reveal) + confidence
// tracking (PRD §9/§53). ---

// testSocraticProfile is testProfile with TeacherMode "socratic" — the
// mode adapters/fakeai keys hint attachment off of (via the rendered
// v3 USER prompt's "Teacher mode: socratic" line).
func testSocraticProfile() session.Profile {
	p := testProfile()
	p.TeacherMode = "socratic"
	return p
}

// socraticFeedback wires a socratic-mode session/document over the
// known-bad conjugation content, requests feedback, and returns the
// resulting single CorrectionView — the common setup every Task 8 test
// below builds on.
func socraticFeedback(t *testing.T, h *testHarness) appfeedback.CorrectionView {
	t.Helper()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testSocraticProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fb, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	if len(fb.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(fb.Corrections), fb.Corrections)
	}
	return fb.Corrections[0]
}

// TestRequestFeedbackSocraticEmitsHintShownEvent pins the hint.shown
// event (fired alongside correction.presented — see RequestFeedback)
// for a socratic-mode correction, and that the initial CorrectionView
// itself already carries the hint.
func TestRequestFeedbackSocraticEmitsHintShownEvent(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	if !cv.HasHint() {
		t.Fatalf("initial view has no hint in socratic mode: %+v", cv)
	}

	var found bool
	for _, ev := range h.events.events {
		if ev.Type == event.TypeHintShown {
			found = true
			if ev.Subject != cv.ID {
				t.Fatalf("hint.shown Subject = %q, want %q", ev.Subject, cv.ID)
			}
		}
	}
	if !found {
		t.Fatalf("expected a hint.shown event, found none: %+v", h.events.events)
	}
}

// TestGetFeedbackPreservesSocraticGate pins this task's stated
// recurring hazard: GetFeedback (Phase 4 Task W items 2/3 — loading a
// past review from the workspace's feedback-history list) must
// reconstruct EXACTLY the same gated CorrectionView a fresh
// RequestFeedback would — the hint still shown, the answer still
// withheld — never a summary that drops HasHint/Status/Revealed and
// lets a re-rendered older correction leak its answer where a fresh
// request would have withheld it. correction.IsGated (not a re-derived
// HasHint()&&Status=="presented"&&!Revealed) is the predicate asserted
// here, matching every other caller in the codebase.
func TestGetFeedbackPreservesSocraticGate(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testSocraticProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	fresh, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}

	got, err := h.svc.GetFeedback(context.Background(), testIdentity, fresh.ID)
	if err != nil {
		t.Fatalf("GetFeedback returned error: %v", err)
	}
	if len(got.Corrections) != 1 {
		t.Fatalf("len(Corrections) = %d, want 1: %+v", len(got.Corrections), got.Corrections)
	}
	cv := got.Corrections[0]
	if !cv.HasHint() {
		t.Fatal("HasHint() = false, want true (fakeai attaches a hint in socratic mode)")
	}
	if cv.Status != "presented" || cv.Revealed {
		t.Fatalf("Status/Revealed = %q/%v, want presented/false", cv.Status, cv.Revealed)
	}
	if !correction.IsGated(cv.HasHint(), cv.Status, cv.Revealed) {
		t.Fatal("correction.IsGated = false, want true — a history read must still withhold this correction's answer")
	}

	// Cross-identity: a different identity's GetFeedback misses with
	// ErrNotFound, same as every other identity-scoped read.
	if _, err := h.svc.GetFeedback(context.Background(), "someone-else", fresh.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetFeedback(cross-identity) err = %v, want storage.ErrNotFound", err)
	}
}

// TestRequestFeedbackNonSocraticOmitsHintShownEvent: the default
// "teacher" mode harness (used throughout this file) must never record
// hint.shown — fakeai never attaches a hint outside socratic mode (see
// that package's own tests), and this pins the service-level
// consequence of that.
func TestRequestFeedbackNonSocraticOmitsHintShownEvent(t *testing.T) {
	h := newTestHarness()
	h.putSession(session.Session{ID: testSessionID, IdentityID: testIdentity, Purpose: "diary", Profile: testProfile()})
	content := "とても面白いでした"
	h.docs.put(writing.Document{ID: testDocID, SessionID: testSessionID, IdentityID: testIdentity, Content: content, Version: 1})

	_, err := h.svc.RequestFeedback(context.Background(), appfeedback.Request{
		Identity: testIdentity, SessionID: testSessionID, DocumentID: testDocID,
		Start: 0, End: len([]rune(content)),
	})
	if err != nil {
		t.Fatalf("RequestFeedback returned error: %v", err)
	}
	for _, ev := range h.events.events {
		if ev.Type == event.TypeHintShown {
			t.Fatalf("recorded an unexpected hint.shown event in non-socratic mode: %+v", ev)
		}
	}
}

// TestRetryCorrectionCorrectFlipsStatusAndRecordsIndependentEvent pins
// the brief's Step 2 "retry correct flips status + event with
// independent=true" scenario: a first-try correct retry (never
// revealed) accepts the correction and records correction.retried with
// Evidence {attempts:1, correct:true, independent:true} — and does NOT
// also record a separate correction.accepted (see RetryCorrection's
// doc comment on why retry-correct is its own signal).
func TestRetryCorrectionCorrectFlipsStatusAndRecordsIndependentEvent(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)
	baseline := len(h.events.events)

	result, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, cv.Replacement)
	if err != nil {
		t.Fatalf("RetryCorrection returned error: %v", err)
	}
	if !result.Correct {
		t.Fatal("Correct = false, want true for a matching retry")
	}
	if result.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", result.Attempts)
	}
	if result.CorrectionView.Status != "accepted" {
		t.Fatalf("CorrectionView.Status = %q, want accepted", result.CorrectionView.Status)
	}

	if len(h.events.events) != baseline+1 {
		t.Fatalf("recorded %d new events, want 1: %+v", len(h.events.events)-baseline, h.events.events[baseline:])
	}
	last := h.events.events[len(h.events.events)-1]
	if last.Type != event.TypeCorrectionRetried {
		t.Fatalf("event Type = %q, want %q", last.Type, event.TypeCorrectionRetried)
	}
	if last.Subject != cv.ID {
		t.Fatalf("event Subject = %q, want %q", last.Subject, cv.ID)
	}
	if last.SessionID == nil || *last.SessionID != testSessionID {
		t.Fatalf("event SessionID = %v, want %q", last.SessionID, testSessionID)
	}
	if got := last.Evidence["attempts"]; got != 1 {
		t.Fatalf("Evidence[attempts] = %v, want 1", got)
	}
	if got := last.Evidence["correct"]; got != true {
		t.Fatalf("Evidence[correct] = %v, want true", got)
	}
	if got := last.Evidence["independent"]; got != true {
		t.Fatalf("Evidence[independent] = %v, want true (never revealed)", got)
	}

	// No separate correction.accepted must have been recorded — retry's
	// own correction.retried is the sole signal for this resolution.
	for _, ev := range h.events.events {
		if ev.Type == event.TypeCorrectionAccepted {
			t.Fatalf("unexpectedly recorded a correction.accepted event alongside correction.retried: %+v", ev)
		}
	}
}

// TestRetryCorrectionEventCarriesResolvedConcepts pins the producer
// side of the correction.retried -> spaced-review contract (Phase 4
// Task 7, PRD §54): the recorded event's Evidence["concepts"] must
// actually contain the correction's resolved concept slug(s) —
// application/retrieval.Consumer schedules spaced review from exactly
// this key (see handleCorrectionRetried's own doc comment on why it
// reads Evidence instead of re-querying GetCorrectionConcepts itself).
// Without this test, deleting the "concepts" entry from RetryCorrection's
// Evidence map compiles and leaves every other suite green — the whole
// correction-retry half of spaced retrieval would go silently dead.
func TestRetryCorrectionEventCarriesResolvedConcepts(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h) // fakeai tags this sentence i-adjective-past (resolved)

	if _, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, cv.Replacement); err != nil {
		t.Fatalf("RetryCorrection returned error: %v", err)
	}

	last := h.events.events[len(h.events.events)-1]
	if last.Type != event.TypeCorrectionRetried {
		t.Fatalf("event Type = %q, want %q", last.Type, event.TypeCorrectionRetried)
	}
	got, ok := last.Evidence["concepts"].([]string)
	if !ok {
		t.Fatalf("Evidence[concepts] = %#v (type %T), want a []string", last.Evidence["concepts"], last.Evidence["concepts"])
	}
	want := []string{"i-adjective-past"}
	if len(got) != len(want) {
		t.Fatalf("Evidence[concepts] = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Evidence[concepts] = %v, want %v", got, want)
		}
	}
}

// TestRetryCorrectionAfterRevealIndependentFalse pins the brief's
// "retry after reveal → independent=false" scenario: RevealCorrection
// first (so the learner has seen the answer), THEN a correct retry —
// still Correct=true (it matches), but independent must be false since
// the answer was revealed before this attempt.
func TestRetryCorrectionAfterRevealIndependentFalse(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	if _, err := h.svc.RevealCorrection(context.Background(), testIdentity, cv.ID); err != nil {
		t.Fatalf("RevealCorrection returned error: %v", err)
	}

	result, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, cv.Replacement)
	if err != nil {
		t.Fatalf("RetryCorrection returned error: %v", err)
	}
	if !result.Correct {
		t.Fatal("Correct = false, want true for a matching retry")
	}

	last := h.events.events[len(h.events.events)-1]
	if last.Type != event.TypeCorrectionRetried {
		t.Fatalf("event Type = %q, want %q", last.Type, event.TypeCorrectionRetried)
	}
	if got := last.Evidence["independent"]; got != false {
		t.Fatalf("Evidence[independent] = %v, want false (answer was revealed)", got)
	}
}

// TestRetryCorrectionWrongAttemptIncrementsAttemptsStatusUnchanged
// pins the brief's "wrong attempt increments attempts, status
// unchanged" scenario, across TWO consecutive wrong attempts (proving
// the count actually accumulates, not just "goes from 0 to 1").
func TestRetryCorrectionWrongAttemptIncrementsAttemptsStatusUnchanged(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	result, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, "面白いです")
	if err != nil {
		t.Fatalf("RetryCorrection returned error: %v", err)
	}
	if result.Correct {
		t.Fatal("Correct = true, want false for a non-matching retry")
	}
	if result.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", result.Attempts)
	}
	if result.CorrectionView.Status != "presented" {
		t.Fatalf("CorrectionView.Status = %q, want unchanged presented", result.CorrectionView.Status)
	}

	result2, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, "まだ違います")
	if err != nil {
		t.Fatalf("RetryCorrection (2nd) returned error: %v", err)
	}
	if result2.Correct {
		t.Fatal("Correct = true on 2nd wrong attempt, want false")
	}
	if result2.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2 (accumulated across two wrong retries)", result2.Attempts)
	}
	if result2.CorrectionView.Status != "presented" {
		t.Fatalf("CorrectionView.Status = %q, want still presented", result2.CorrectionView.Status)
	}
}

// TestRetryCorrectionAttemptIsTrimmed: surrounding whitespace on the
// submitted attempt must not defeat a correct retry — form input
// commonly carries a trailing newline/space a learner didn't intend.
func TestRetryCorrectionAttemptIsTrimmed(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	result, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, "  "+cv.Replacement+"\n")
	if err != nil {
		t.Fatalf("RetryCorrection returned error: %v", err)
	}
	if !result.Correct {
		t.Fatal("Correct = false, want true once surrounding whitespace is trimmed")
	}
}

// TestRetryCorrectionCrossIdentityReturnsErrNotFound pins the brief's
// "cross-identity retry → ErrNotFound" scenario.
func TestRetryCorrectionCrossIdentityReturnsErrNotFound(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	_, err := h.svc.RetryCorrection(context.Background(), "learner-mallory", cv.ID, cv.Replacement)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
	if h.repo.corrections[cv.ID].Attempts != 0 {
		t.Fatalf("Attempts = %d, want unchanged 0 after a cross-identity attempt", h.repo.corrections[cv.ID].Attempts)
	}
}

// TestRetryCorrectionOnAlreadyAcceptedReturnsErrNotFound: once a
// correction has left Status "presented" (here, via a first correct
// retry), a further retry attempt has nothing to act on — the retry
// endpoint's WHERE clause (status = 'presented') excludes it, same
// ErrNotFound miss shape as everything else in this family.
func TestRetryCorrectionOnAlreadyAcceptedReturnsErrNotFound(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	if _, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, cv.Replacement); err != nil {
		t.Fatalf("first RetryCorrection returned error: %v", err)
	}

	_, err := h.svc.RetryCorrection(context.Background(), testIdentity, cv.ID, cv.Replacement)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("retry on an already-accepted correction: err = %v, want storage.ErrNotFound", err)
	}
}

// TestRevealCorrectionRecordsAnswerRevealedEvent pins the reveal
// endpoint's service-level contract: Status stays "presented" (reveal
// alone doesn't resolve the correction — the learner still has to
// retry or explicitly accept), Revealed flips true, and answer.revealed
// is recorded.
func TestRevealCorrectionRecordsAnswerRevealedEvent(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)
	baseline := len(h.events.events)

	view, err := h.svc.RevealCorrection(context.Background(), testIdentity, cv.ID)
	if err != nil {
		t.Fatalf("RevealCorrection returned error: %v", err)
	}
	if !view.Revealed {
		t.Fatal("Revealed = false, want true")
	}
	if view.Status != "presented" {
		t.Fatalf("Status = %q, want unchanged presented", view.Status)
	}

	if len(h.events.events) != baseline+1 {
		t.Fatalf("recorded %d new events, want 1", len(h.events.events)-baseline)
	}
	last := h.events.events[len(h.events.events)-1]
	if last.Type != event.TypeAnswerRevealed {
		t.Fatalf("event Type = %q, want %q", last.Type, event.TypeAnswerRevealed)
	}
	if last.Subject != cv.ID {
		t.Fatalf("event Subject = %q, want %q", last.Subject, cv.ID)
	}
	if last.SessionID == nil || *last.SessionID != testSessionID {
		t.Fatalf("event SessionID = %v, want %q", last.SessionID, testSessionID)
	}
}

// TestRevealCorrectionCrossIdentityReturnsErrNotFound mirrors the
// retry-side cross-identity pin, for reveal.
func TestRevealCorrectionCrossIdentityReturnsErrNotFound(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	_, err := h.svc.RevealCorrection(context.Background(), "learner-mallory", cv.ID)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
	if h.repo.corrections[cv.ID].Revealed {
		t.Fatal("Revealed = true after a cross-identity attempt, want unchanged false")
	}
}

// TestRecordConfidenceValidRangePersistsAndRecordsEvent pins the
// brief's "confidence 1..5 persisted + event" scenario.
func TestRecordConfidenceValidRangePersistsAndRecordsEvent(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)
	baseline := len(h.events.events)

	if err := h.svc.RecordConfidence(context.Background(), testIdentity, cv.ID, 4); err != nil {
		t.Fatalf("RecordConfidence returned error: %v", err)
	}

	rec := h.repo.corrections[cv.ID]
	if rec.Confidence == nil || *rec.Confidence != 4 {
		t.Fatalf("persisted Confidence = %v, want pointer to 4", rec.Confidence)
	}

	if len(h.events.events) != baseline+1 {
		t.Fatalf("recorded %d new events, want 1", len(h.events.events)-baseline)
	}
	last := h.events.events[len(h.events.events)-1]
	if last.Type != event.TypeConfidenceRecorded {
		t.Fatalf("event Type = %q, want %q", last.Type, event.TypeConfidenceRecorded)
	}
	if last.Subject != cv.ID {
		t.Fatalf("event Subject = %q, want %q", last.Subject, cv.ID)
	}
	if got := last.Evidence["confidence"]; got != 4 {
		t.Fatalf("Evidence[confidence] = %v, want 4", got)
	}
}

// TestRecordConfidenceOutOfRangeReturnsErrorAndRecordsNoEvent pins the
// brief's "0/6 → error" scenario for both boundary violations, and
// that neither call reaches the repository/recorder at all.
func TestRecordConfidenceOutOfRangeReturnsErrorAndRecordsNoEvent(t *testing.T) {
	for _, bad := range []int{0, 6, -1, 100} {
		h := newTestHarness()
		cv := socraticFeedback(t, h)
		baseline := len(h.events.events)

		err := h.svc.RecordConfidence(context.Background(), testIdentity, cv.ID, bad)
		if !errors.Is(err, appfeedback.ErrInvalidConfidence) {
			t.Fatalf("confidence=%d: err = %v, want ErrInvalidConfidence", bad, err)
		}
		if len(h.events.events) != baseline {
			t.Fatalf("confidence=%d: recorded %d new events, want 0", bad, len(h.events.events)-baseline)
		}
		if h.repo.corrections[cv.ID].Confidence != nil {
			t.Fatalf("confidence=%d: persisted Confidence = %v, want unchanged nil", bad, h.repo.corrections[cv.ID].Confidence)
		}
	}
}

// TestRecordConfidenceCrossIdentityReturnsErrNotFound mirrors the
// retry/reveal-side cross-identity pin, for confidence.
func TestRecordConfidenceCrossIdentityReturnsErrNotFound(t *testing.T) {
	h := newTestHarness()
	cv := socraticFeedback(t, h)

	err := h.svc.RecordConfidence(context.Background(), "learner-mallory", cv.ID, 3)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
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
