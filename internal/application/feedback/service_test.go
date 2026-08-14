package feedback_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
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

// fakeGrammarRepo is an in-memory storage.GrammarRepository: only
// ListConcepts is exercised by the feedback pipeline (it builds the
// teacher.feedback.v2 prompt's candidate list and the known-slug set
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
	svc      *appfeedback.Service
	sessions *fakeSessionRepo
	docs     *fakeDocRepo
	repo     *fakeFeedbackRepo
	grammar  *fakeGrammarRepo
	events   *fakeEventStore
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
	events := &fakeEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	t := teacher.New(gen)
	svc := appfeedback.NewService(sessions, docs, repo, grammarRepo, t, rec)
	return &testHarness{svc: svc, sessions: sessions, docs: docs, repo: repo, grammar: grammarRepo, events: events}
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
