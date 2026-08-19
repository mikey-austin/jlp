package tools_test

import (
	"context"
	"sync"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// This file collects small, hand-written in-memory fakes for every
// storage.* port the tool constructors under test need to wire a real
// application service against — mirroring the "fake<Thing>Repo" doubles
// each application/*/service_test.go already uses (see e.g.
// application/anki/service_test.go's fakeAnkiCardRepo), just gathered
// in one place since internal/tools's tests exercise several services
// at once. Every fake is minimal: it implements exactly the behavior
// the tools under test actually exercise, not the full realistic
// semantics of the real postgres adapter.

// --- sessions ---

type fakeSessionRepo struct {
	mu   sync.Mutex
	byID map[session.ID]session.Session
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byID: map[session.ID]session.Session{}}
}

func (f *fakeSessionRepo) Create(_ context.Context, s session.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[s.ID] = s
	return nil
}

func (f *fakeSessionRepo) Get(_ context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[id]
	if !ok || s.IdentityID != identity {
		return session.Session{}, storage.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessionRepo) List(_ context.Context, identity learner.IdentityID) ([]session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []session.Session
	for _, s := range f.byID {
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

// --- documents ---

type fakeDocumentRepo struct {
	mu        sync.Mutex
	bySession map[session.ID]writing.Document
}

func newFakeDocumentRepo() *fakeDocumentRepo {
	return &fakeDocumentRepo{bySession: map[session.ID]writing.Document{}}
}

func (f *fakeDocumentRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.bySession[sid]; ok {
		return d, false, nil
	}
	d := writing.Document{ID: writing.DocumentID("doc-" + string(sid)), SessionID: sid, IdentityID: identity, Content: "", Version: 1, UpdatedAt: time.Now().UTC()}
	f.bySession[sid] = d
	return d, true, nil
}

func (f *fakeDocumentRepo) Save(_ context.Context, _ learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sid, d := range f.bySession {
		if d.ID == id {
			d.Content = content
			d.Version++
			d.UpdatedAt = time.Now().UTC()
			f.bySession[sid] = d
			return d, nil
		}
	}
	return writing.Document{}, storage.ErrNotFound
}

func (f *fakeDocumentRepo) Get(_ context.Context, _ learner.IdentityID, id writing.DocumentID) (writing.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.bySession {
		if d.ID == id {
			return d, nil
		}
	}
	return writing.Document{}, storage.ErrNotFound
}

func (f *fakeDocumentRepo) ListVersions(context.Context, learner.IdentityID, writing.DocumentID, int) ([]writing.Document, error) {
	return nil, nil
}

// seed pre-populates a session's document content directly (bypassing
// GetOrCreateForSession's create-on-first-open path) for tests that
// need get_recent_writing to see existing content.
func (f *fakeDocumentRepo) seed(identity learner.IdentityID, sid session.ID, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bySession[sid] = writing.Document{ID: writing.DocumentID("doc-" + string(sid)), SessionID: sid, IdentityID: identity, Content: content, Version: 3, UpdatedAt: time.Now().UTC()}
}

// --- vocabulary ---

type fakeVocabRepo struct {
	items []vocabulary.Item
}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	return vocabulary.Item{}, false, nil
}
func (f *fakeVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) List(_ context.Context, identity learner.IdentityID, _ string) ([]vocabulary.Item, error) {
	var out []vocabulary.Item
	for _, it := range f.items {
		if it.IdentityID == identity {
			out = append(out, it)
		}
	}
	return out, nil
}
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	return nil, nil
}
func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	return nil, nil
}
func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) GetByExpressions(context.Context, learner.IdentityID, []string) ([]vocabulary.Item, error) {
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

// --- identities ---

type fakeIdentityRepo struct {
	byID map[learner.IdentityID]learner.Identity
}

func newFakeIdentityRepo() *fakeIdentityRepo {
	return &fakeIdentityRepo{byID: map[learner.IdentityID]learner.Identity{}}
}
func (f *fakeIdentityRepo) Upsert(_ context.Context, id learner.Identity) error {
	f.byID[id.ID] = id
	return nil
}
func (f *fakeIdentityRepo) Get(_ context.Context, id learner.IdentityID) (learner.Identity, error) {
	v, ok := f.byID[id]
	if !ok {
		return learner.Identity{}, storage.ErrNotFound
	}
	return v, nil
}
func (f *fakeIdentityRepo) ListIdentities(context.Context) ([]learner.Identity, error) {
	return nil, nil
}

// --- observations ---

type fakeObservationRepo struct {
	byIdentity map[learner.IdentityID][]learnermodel.Observation
}

func newFakeObservationRepo() *fakeObservationRepo {
	return &fakeObservationRepo{byIdentity: map[learner.IdentityID][]learnermodel.Observation{}}
}
func (f *fakeObservationRepo) Upsert(_ context.Context, o learnermodel.Observation) error {
	f.byIdentity[o.IdentityID] = append(f.byIdentity[o.IdentityID], o)
	return nil
}
func (f *fakeObservationRepo) List(_ context.Context, identity learner.IdentityID) ([]learnermodel.Observation, error) {
	return f.byIdentity[identity], nil
}
func (f *fakeObservationRepo) DeleteAll(_ context.Context, identity learner.IdentityID) error {
	delete(f.byIdentity, identity)
	return nil
}

// --- feedback / corrections ---

type fakeFeedbackRepo struct {
	mu          sync.Mutex
	corrections map[string]storage.CorrectionRecord
	// owners is a side map from correction ID to owning identity, since
	// storage.CorrectionRecord itself carries no IdentityID field —
	// GetCorrection/RecentCorrections below consult it to decide scope,
	// mirroring the real postgres adapter's join-through-feedback_requests.
	owners map[string]learner.IdentityID
	// order preserves insertion order so RecentCorrections has a
	// deterministic "newest first" that a test can rely on: entries
	// appended later are considered newer.
	order []string
}

func newFakeFeedbackRepo() *fakeFeedbackRepo {
	return &fakeFeedbackRepo{corrections: map[string]storage.CorrectionRecord{}, owners: map[string]learner.IdentityID{}}
}

// seed registers a correction under identity's ownership.
func (f *fakeFeedbackRepo) seed(identity learner.IdentityID, c storage.CorrectionRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners[c.ID] = identity
	f.corrections[c.ID] = c
	f.order = append(f.order, c.ID)
}

func (f *fakeFeedbackRepo) InsertFeedback(context.Context, storage.FeedbackRecord, []storage.CorrectionRecord, map[string][]storage.ConceptTag) error {
	return nil
}
func (f *fakeFeedbackRepo) UpdateCorrectionStatus(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	return storage.CorrectionRecord{}, nil
}
func (f *fakeFeedbackRepo) GetCorrectionConcepts(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeFeedbackRepo) RetryCorrection(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	return storage.CorrectionRecord{}, nil
}
func (f *fakeFeedbackRepo) RevealCorrection(context.Context, learner.IdentityID, string) (storage.CorrectionRecord, error) {
	return storage.CorrectionRecord{}, nil
}
func (f *fakeFeedbackRepo) RecordConfidence(context.Context, learner.IdentityID, string, int) (storage.CorrectionRecord, error) {
	return storage.CorrectionRecord{}, nil
}
func (f *fakeFeedbackRepo) ListForSession(context.Context, learner.IdentityID, session.ID) ([]storage.FeedbackSummary, error) {
	return nil, nil
}
func (f *fakeFeedbackRepo) GetFeedback(context.Context, learner.IdentityID, string) (storage.FeedbackDetail, []storage.CorrectionRecord, error) {
	return storage.FeedbackDetail{}, nil, nil
}
func (f *fakeFeedbackRepo) GetCorrection(_ context.Context, identity learner.IdentityID, correctionID string) (storage.CorrectionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.corrections[correctionID]
	if !ok || f.owners[correctionID] != identity {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	return c, nil
}
func (f *fakeFeedbackRepo) RecentCorrections(_ context.Context, identity learner.IdentityID, limit int) ([]storage.CorrectionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storage.CorrectionRecord
	for i := len(f.order) - 1; i >= 0; i-- {
		id := f.order[i]
		if f.owners[id] != identity {
			continue
		}
		out = append(out, f.corrections[id])
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// --- priorities ---

type fakePriorityRepo struct {
	byIdentity map[learner.IdentityID][]storage.Priority
}

func newFakePriorityRepo() *fakePriorityRepo {
	return &fakePriorityRepo{byIdentity: map[learner.IdentityID][]storage.Priority{}}
}
func (f *fakePriorityRepo) ReplaceAll(_ context.Context, identity learner.IdentityID, ps []storage.Priority) error {
	f.byIdentity[identity] = ps
	return nil
}
func (f *fakePriorityRepo) Top(_ context.Context, identity learner.IdentityID, limit int) ([]storage.Priority, error) {
	ps := f.byIdentity[identity]
	if len(ps) > limit {
		ps = ps[:limit]
	}
	return ps, nil
}

// --- grammar ---

type fakeGrammarRepo struct {
	concepts  []grammar.Concept
	statsByID map[learner.IdentityID][]storage.ConceptStat
}

func newFakeGrammarRepo(concepts []grammar.Concept) *fakeGrammarRepo {
	return &fakeGrammarRepo{concepts: concepts, statsByID: map[learner.IdentityID][]storage.ConceptStat{}}
}
func (f *fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error { return nil }
func (f *fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	return f.concepts, nil
}
func (f *fakeGrammarRepo) GetConcept(_ context.Context, slug string) (grammar.Concept, error) {
	for _, c := range f.concepts {
		if c.Slug == slug {
			return c, nil
		}
	}
	return grammar.Concept{}, storage.ErrNotFound
}
func (f *fakeGrammarRepo) ConceptStats(_ context.Context, identity learner.IdentityID) ([]storage.ConceptStat, error) {
	return f.statsByID[identity], nil
}
func (f *fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	return nil, nil
}

// --- exercises ---

type fakeExerciseRepo struct {
	mu    sync.Mutex
	byID  map[string]exercise.Exercise
	owner map[string]learner.IdentityID
}

func newFakeExerciseRepo() *fakeExerciseRepo {
	return &fakeExerciseRepo{byID: map[string]exercise.Exercise{}, owner: map[string]learner.IdentityID{}}
}
func (f *fakeExerciseRepo) Create(_ context.Context, ex exercise.Exercise) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[ex.ID] = ex
	f.owner[ex.ID] = ex.IdentityID
	return nil
}
func (f *fakeExerciseRepo) Get(_ context.Context, identity learner.IdentityID, id string) (exercise.Exercise, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ex, ok := f.byID[id]
	if !ok || f.owner[id] != identity {
		return exercise.Exercise{}, storage.ErrNotFound
	}
	return ex, nil
}
func (f *fakeExerciseRepo) RecordAttempt(context.Context, storage.ExerciseAttempt) error { return nil }

// --- lessons ---

type fakeLessonRepo struct {
	mu   sync.Mutex
	byID map[string]storage.Lesson
}

func newFakeLessonRepo() *fakeLessonRepo { return &fakeLessonRepo{byID: map[string]storage.Lesson{}} }
func (f *fakeLessonRepo) Insert(_ context.Context, l storage.Lesson) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[l.ID] = l
	return nil
}
func (f *fakeLessonRepo) List(context.Context, learner.IdentityID) ([]storage.Lesson, error) {
	return nil, nil
}
func (f *fakeLessonRepo) Get(_ context.Context, identity learner.IdentityID, id string) (storage.Lesson, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.byID[id]
	if !ok || l.IdentityID != identity {
		return storage.Lesson{}, storage.ErrNotFound
	}
	return l, nil
}
func (f *fakeLessonRepo) CompleteWithObservation(context.Context, learner.IdentityID, string, storage.LessonObservation, time.Time) (storage.Lesson, error) {
	return storage.Lesson{}, nil
}
func (f *fakeLessonRepo) Observations(context.Context, learner.IdentityID, string) ([]storage.LessonObservation, error) {
	return nil, nil
}

// Soft delete (Phase 4 Task D) — unused by these tests; present to satisfy the port.
func (f *fakeLessonRepo) SoftDelete(context.Context, learner.IdentityID, string, time.Time) error {
	return nil
}
func (f *fakeLessonRepo) Restore(context.Context, learner.IdentityID, string) error {
	return nil
}

// --- anki cards ---

type fakeAnkiCardRepo struct {
	mu   sync.Mutex
	byID map[string]storage.AnkiCard
}

func newFakeAnkiCardRepo() *fakeAnkiCardRepo {
	return &fakeAnkiCardRepo{byID: map[string]storage.AnkiCard{}}
}
func (f *fakeAnkiCardRepo) Insert(_ context.Context, c storage.AnkiCard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[c.ID] = c
	return nil
}
func (f *fakeAnkiCardRepo) List(context.Context, learner.IdentityID, string) ([]storage.AnkiCard, error) {
	return nil, nil
}
func (f *fakeAnkiCardRepo) UpdateStatus(context.Context, learner.IdentityID, string, string) (storage.AnkiCard, error) {
	return storage.AnkiCard{}, nil
}
func (f *fakeAnkiCardRepo) TakeApprovedForExport(context.Context, learner.IdentityID, time.Time) ([]storage.AnkiCard, error) {
	return nil, nil
}

// --- learning events ---

type fakeEventRepo struct {
	mu     sync.Mutex
	events []event.LearningEvent
}

func newFakeEventRepo() *fakeEventRepo { return &fakeEventRepo{} }
func (f *fakeEventRepo) Append(_ context.Context, ev event.LearningEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}
func (f *fakeEventRepo) ListRecent(_ context.Context, identity learner.IdentityID, _ *session.ID, limit int) ([]event.LearningEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []event.LearningEvent
	for i := len(f.events) - 1; i >= 0; i-- {
		if f.events[i].IdentityID == identity {
			out = append(out, f.events[i])
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (f *fakeEventRepo) ListAll(_ context.Context, identity learner.IdentityID) ([]event.LearningEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []event.LearningEvent
	for _, ev := range f.events {
		if ev.IdentityID == identity {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeEventRepo) snapshot() []event.LearningEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event.LearningEvent, len(f.events))
	copy(out, f.events)
	return out
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
