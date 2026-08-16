package channel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	"github.com/mikeyaustin/jlp/internal/agent/drill"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	appchannel "github.com/mikeyaustin/jlp/internal/application/channel"
	"github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/channels"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// --- in-memory fakes, mirroring application/feedback/service_test.go's
// and application/practice/service_test.go's established "real
// collaborators, fake edges" shape. ---

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

// fakeDocRepo is a fully functional in-memory storage.DocumentRepository
// (unlike feedback/service_test.go's own double, which panics on
// GetOrCreateForSession/Save — application/channel.Service is the first
// application-layer caller in this codebase to actually drive those two
// methods itself, rather than through application/writing).
type fakeDocRepo struct {
	docs map[string]writing.Document // key: identity+"/"+id
	seq  int
}

func newFakeDocRepo() *fakeDocRepo {
	return &fakeDocRepo{docs: map[string]writing.Document{}}
}

func docKey(identity learner.IdentityID, id writing.DocumentID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeDocRepo) GetOrCreateForSession(_ context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, bool, error) {
	for _, d := range f.docs {
		if d.IdentityID == identity && d.SessionID == sid {
			return d, false, nil
		}
	}
	f.seq++
	doc := writing.Document{
		ID:         writing.DocumentID(sessKey(identity, sid) + "-doc"),
		SessionID:  sid,
		IdentityID: identity,
		Content:    "",
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
	}
	f.docs[docKey(identity, doc.ID)] = doc
	return doc, true, nil
}

func (f *fakeDocRepo) Save(_ context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	key := docKey(identity, id)
	doc, ok := f.docs[key]
	if !ok {
		return writing.Document{}, storage.ErrNotFound
	}
	doc.Content = content
	doc.Version++
	doc.UpdatedAt = time.Now().UTC()
	f.docs[key] = doc
	return doc, nil
}

func (f *fakeDocRepo) Get(_ context.Context, identity learner.IdentityID, id writing.DocumentID) (writing.Document, error) {
	doc, ok := f.docs[docKey(identity, id)]
	if !ok {
		return writing.Document{}, storage.ErrNotFound
	}
	return doc, nil
}

func (f *fakeDocRepo) ListVersions(context.Context, learner.IdentityID, writing.DocumentID, int) ([]writing.Document, error) {
	panic("not used by channel service tests")
}

// fakeIdentityRepo is a fully functional in-memory
// storage.IdentityRepository — application/channel.Service.
// ensureIdentity is the first caller in this codebase to drive Upsert
// from outside a seed/CLI path, so (unlike the other fakes above) there
// is no existing test double to mirror.
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
	ident, ok := f.byID[id]
	if !ok {
		return learner.Identity{}, storage.ErrNotFound
	}
	return ident, nil
}

func (f *fakeIdentityRepo) ListIdentities(context.Context) ([]learner.Identity, error) {
	out := make([]learner.Identity, 0, len(f.byID))
	for _, ident := range f.byID {
		out = append(out, ident)
	}
	return out, nil
}

// fakeFeedbackRepo mirrors application/feedback/service_test.go's own
// double verbatim — only InsertFeedback is exercised by these tests,
// every other method panics if actually called (this package's tests
// never accept/reject/retry/reveal a correction, or read one back).
type fakeFeedbackRepo struct {
	feedback    map[string]storage.FeedbackRecord
	corrections map[string]storage.CorrectionRecord
}

func newFakeFeedbackRepo() *fakeFeedbackRepo {
	return &fakeFeedbackRepo{
		feedback:    map[string]storage.FeedbackRecord{},
		corrections: map[string]storage.CorrectionRecord{},
	}
}

func (f *fakeFeedbackRepo) InsertFeedback(_ context.Context, rec storage.FeedbackRecord, corrections []storage.CorrectionRecord, _ map[string][]storage.ConceptTag) error {
	f.feedback[rec.ID] = rec
	for _, c := range corrections {
		f.corrections[c.ID] = c
	}
	return nil
}

func (f *fakeFeedbackRepo) UpdateCorrectionStatus(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}
func (f *fakeFeedbackRepo) RetryCorrection(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}
func (f *fakeFeedbackRepo) RevealCorrection(context.Context, learner.IdentityID, string) (storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}
func (f *fakeFeedbackRepo) RecordConfidence(context.Context, learner.IdentityID, string, int) (storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}
func (f *fakeFeedbackRepo) GetCorrection(context.Context, learner.IdentityID, string) (storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}
func (f *fakeFeedbackRepo) RecentCorrections(context.Context, learner.IdentityID, int) ([]storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}
func (f *fakeFeedbackRepo) GetCorrectionConcepts(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeFeedbackRepo) ListForSession(context.Context, learner.IdentityID, session.ID) ([]storage.FeedbackSummary, error) {
	panic("not used by channel service tests")
}
func (f *fakeFeedbackRepo) GetFeedback(context.Context, learner.IdentityID, string) (storage.FeedbackDetail, []storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}

// fakeGrammarRepo mirrors application/practice/service_test.go's own
// double: ListConcepts backs Start's random-fallback path (this
// harness's PriorityRepository is always empty, so TopConcept always
// misses), GetConcept is unused (TopConcept never reaches it) and
// panics if it ever is.
type fakeGrammarRepo struct {
	concepts []grammar.Concept
}

func (f *fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by channel service tests")
}
func (f *fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	return f.concepts, nil
}
func (f *fakeGrammarRepo) GetConcept(context.Context, string) (grammar.Concept, error) {
	panic("not used by channel service tests")
}
func (f *fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by channel service tests")
}
func (f *fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by channel service tests")
}

// fakePriorityRepo always reports no priorities — TopConcept then
// misses and Start falls back to the random catalog concept, same
// "canned empty data" shape feedback/service_test.go's own double uses.
type fakePriorityRepo struct{}

func (f *fakePriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by channel service tests")
}
func (f *fakePriorityRepo) Top(context.Context, learner.IdentityID, int) ([]storage.Priority, error) {
	return nil, nil
}

// fakeObsRepo exists only to satisfy planner.NewPlanner's signature.
type fakeObsRepo struct{}

func (f *fakeObsRepo) Upsert(context.Context, learnermodel.Observation) error {
	panic("not used by channel service tests")
}
func (f *fakeObsRepo) List(context.Context, learner.IdentityID) ([]learnermodel.Observation, error) {
	panic("not used by channel service tests")
}
func (f *fakeObsRepo) DeleteAll(context.Context, learner.IdentityID) error {
	panic("not used by channel service tests")
}

// fakeVocabRepo backs both the planner's ActivationCandidates (called
// on every feedback round) and appvocabulary.Service's DetectProduction
// (called at the end of every RequestFeedback) — an empty item set for
// both is enough for these tests, which are not about vocabulary at all.
type fakeVocabRepo struct{}

func (f *fakeVocabRepo) UpsertOnLookup(context.Context, learner.IdentityID, string, string, string, string, string, vocabulary.Kind, string, time.Time) (vocabulary.Item, bool, error) {
	panic("not used by channel service tests")
}
func (f *fakeVocabRepo) RecordProduction(context.Context, learner.IdentityID, string, bool, time.Time) error {
	return nil
}
func (f *fakeVocabRepo) List(context.Context, learner.IdentityID, string) ([]vocabulary.Item, error) {
	panic("not used by channel service tests")
}
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	return nil, nil
}
func (f *fakeVocabRepo) AllExpressions(context.Context, learner.IdentityID) (map[string]string, error) {
	return map[string]string{}, nil
}
func (f *fakeVocabRepo) SeedBank(context.Context, learner.IdentityID, []vocabulary.BankEntry, time.Time) error {
	panic("not used by channel service tests")
}
func (f *fakeVocabRepo) BulkUpsertWords(context.Context, learner.IdentityID, []storage.WordInput, time.Time) (int, error) {
	panic("not used by channel service tests")
}

// fakeExerciseRepo mirrors application/practice/service_test.go's own
// double.
type fakeExerciseRepo struct {
	byID     map[string]exercise.Exercise
	attempts []storage.ExerciseAttempt
}

func newFakeExerciseRepo() *fakeExerciseRepo {
	return &fakeExerciseRepo{byID: map[string]exercise.Exercise{}}
}

func (f *fakeExerciseRepo) Create(_ context.Context, ex exercise.Exercise) error {
	f.byID[ex.ID] = ex
	return nil
}

func (f *fakeExerciseRepo) Get(_ context.Context, identity learner.IdentityID, id string) (exercise.Exercise, error) {
	ex, ok := f.byID[id]
	if !ok || ex.IdentityID != identity {
		return exercise.Exercise{}, storage.ErrNotFound
	}
	return ex, nil
}

func (f *fakeExerciseRepo) RecordAttempt(_ context.Context, at storage.ExerciseAttempt) error {
	f.attempts = append(f.attempts, at)
	return nil
}

// fakeEventStore is an in-memory storage.LearningEventRepository that
// actually records every Append call, wired into the harness's REAL
// learning.Recorder so tests can assert exactly which events (or how
// many — the unmapped-sender test cares about the COUNT, not the
// content) got recorded.
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

// --- harness ---

var iAdjectivePastConcept = grammar.Concept{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5}

type testHarness struct {
	svc        *appchannel.Service
	sessions   *fakeSessionRepo
	docs       *fakeDocRepo
	identities *fakeIdentityRepo
	feedback   *fakeFeedbackRepo
	exercises  *fakeExerciseRepo
	events     *fakeEventStore
}

// newTestHarness wires a real application/channel.Service over every
// real collaborator service (sessions/feedback/practice) it composes,
// each in turn wired over in-memory fakes and fakeai (deterministic, no
// network) — the same "real collaborators, fake edges" shape
// application/feedback's and application/practice's own test harnesses
// use. allowFrom is the raw APP_CHANNELS_ALLOWFROM value each test
// supplies (see config.Channels.AllowFrom's doc comment for its shape).
func newTestHarness(allowFrom string) *testHarness {
	sessionRepo := newFakeSessionRepo()
	docRepo := newFakeDocRepo()
	identityRepo := newFakeIdentityRepo()
	feedbackRepo := newFakeFeedbackRepo()
	grammarRepo := &fakeGrammarRepo{concepts: []grammar.Concept{iAdjectivePastConcept}}
	prioRepo := &fakePriorityRepo{}
	events := &fakeEventStore{}
	bus := inprocbus.New()
	rec := learning.NewRecorder(events, bus)
	vocabRepo := &fakeVocabRepo{}
	vocabSvc := appvocabulary.NewService(vocabRepo, rec)
	teachingPlanner := planner.NewPlanner(&fakeObsRepo{}, events, grammarRepo, prioRepo, vocabRepo, time.Now)

	sessionsSvc := sessions.NewService(sessionRepo)
	teacherAgent := teacher.New(fakeai.New())
	feedbackSvc := feedback.NewService(sessionRepo, docRepo, feedbackRepo, grammarRepo, prioRepo, teachingPlanner, vocabSvc, teacherAgent, rec, false, nil)

	exerciseRepo := newFakeExerciseRepo()
	drillAgent := drill.New(fakeai.New())
	practiceSvc := practice.NewService(exerciseRepo, drillAgent, teachingPlanner, grammarRepo, rec)

	svc := appchannel.NewService(sessionsSvc, feedbackSvc, practiceSvc, docRepo, identityRepo, config.Channels{AllowFrom: allowFrom})

	return &testHarness{
		svc: svc, sessions: sessionRepo, docs: docRepo, identities: identityRepo,
		feedback: feedbackRepo, exercises: exerciseRepo, events: events,
	}
}

// (1) Japanese text → a correction, rendered compact, for a mapped
// sender's default (non-socratic) session: the replacement IS shown,
// since nothing gates it.
func TestHandleJapaneseTextReturnsCompactCorrection(t *testing.T) {
	h := newTestHarness("slack:U1=learner-a")
	in := channels.Inbound{Channel: "slack", ExternalID: "U1", Text: "とても面白いでした", ThreadID: "t1"}

	out, err := h.svc.Handle(context.Background(), in)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if out.ThreadID != "t1" {
		t.Fatalf("ThreadID = %q, want the inbound thread echoed back", out.ThreadID)
	}
	if !strings.Contains(out.Text, "面白いでした") || !strings.Contains(out.Text, "面白かったです") {
		t.Fatalf("reply = %q, want both the original and the corrected text (non-gated correction)", out.Text)
	}

	// A channel session was created for this sender, titled per PRD §20's
	// example shape.
	sess, err := findSessionByTitle(h.sessions, "learner-a", "Slack — U1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Purpose != "channel" {
		t.Fatalf("session Purpose = %q, want \"channel\"", sess.Purpose)
	}

	// Persisted: one feedback record, one correction, and the identity
	// was auto-provisioned.
	if len(h.feedback.feedback) != 1 {
		t.Fatalf("persisted feedback count = %d, want 1", len(h.feedback.feedback))
	}
	if _, ok := h.identities.byID[learner.IdentityID("learner-a")]; !ok {
		t.Fatal("identity was not auto-provisioned on first contact")
	}
	if len(h.events.events) == 0 {
		t.Fatal("no events were recorded for a mapped sender's correction")
	}
}

// (2) THE critical socratic-gate pin: a gated correction's channel
// reply must contain the hint and must NOT contain the withheld
// replacement text anywhere. Pre-seeding a socratic-mode session (so
// fakeai attaches a hint) before Handle runs reuses
// getOrCreateChannelSession's own "reuse an existing session by title"
// path.
func TestHandleGatedCorrectionShowsHintNotAnswer(t *testing.T) {
	h := newTestHarness("slack:U2=learner-b")
	socraticSession := session.Session{
		ID:         session.ID("sess-socratic"),
		IdentityID: "learner-b",
		Title:      "Slack — U2",
		Purpose:    "channel",
		Profile: session.Profile{
			TeacherMode:         "socratic",
			ExplanationLanguage: "both",
			Strictness:          "balanced",
		},
	}
	if err := h.sessions.Create(context.Background(), socraticSession); err != nil {
		t.Fatal(err)
	}

	in := channels.Inbound{Channel: "slack", ExternalID: "U2", Text: "とても面白いでした", ThreadID: "t2"}
	out, err := h.svc.Handle(context.Background(), in)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	// Precondition: fakeai actually attached a socratic hint to the one
	// persisted correction — otherwise this test would pass vacuously.
	if len(h.feedback.corrections) != 1 {
		t.Fatalf("persisted corrections = %d, want 1", len(h.feedback.corrections))
	}
	var rec storage.CorrectionRecord
	for _, c := range h.feedback.corrections {
		rec = c
	}
	if rec.HintJA == "" && rec.HintEN == "" {
		t.Fatal("precondition failed: fakeai did not attach a socratic hint for a socratic-mode session")
	}
	if !rec.IsGated() {
		t.Fatalf("precondition failed: persisted correction is not gated (Status=%q Revealed=%v)", rec.Status, rec.Revealed)
	}

	// The actual pin: the reply carries the hint but never the withheld
	// replacement text.
	if strings.Contains(out.Text, rec.Replacement) {
		t.Fatalf("reply leaked the gated correction's answer %q: %q", rec.Replacement, out.Text)
	}
	hint := rec.HintEN
	if hint == "" {
		hint = rec.HintJA
	}
	if !strings.Contains(out.Text, hint) {
		t.Fatalf("reply = %q, want it to contain the hint %q", out.Text, hint)
	}
}

// (3) "practice" starts an exercise; the NEXT message is scored as the
// answer.
func TestHandlePracticeThenAnswer(t *testing.T) {
	h := newTestHarness("slack:U3=learner-c")

	startOut, err := h.svc.Handle(context.Background(), channels.Inbound{Channel: "slack", ExternalID: "U3", Text: "practice", ThreadID: "t3"})
	if err != nil {
		t.Fatalf("Handle(practice) returned error: %v", err)
	}
	if startOut.Text == "" {
		t.Fatal("practice start reply was empty")
	}
	if len(h.exercises.byID) != 1 {
		t.Fatalf("persisted exercises = %d, want 1", len(h.exercises.byID))
	}
	var ex exercise.Exercise
	for _, e := range h.exercises.byID {
		ex = e
	}
	if !strings.Contains(startOut.Text, ex.Prompt) {
		t.Fatalf("practice start reply = %q, want it to contain the exercise prompt %q", startOut.Text, ex.Prompt)
	}

	answerOut, err := h.svc.Handle(context.Background(), channels.Inbound{Channel: "slack", ExternalID: "U3", Text: ex.Answer, ThreadID: "t3"})
	if err != nil {
		t.Fatalf("Handle(answer) returned error: %v", err)
	}
	if !strings.HasPrefix(answerOut.Text, "✓") {
		t.Fatalf("answer reply = %q, want a correct (✓) verdict for the exercise's own canonical Answer", answerOut.Text)
	}
	if len(h.exercises.attempts) != 1 {
		t.Fatalf("recorded attempts = %d, want 1", len(h.exercises.attempts))
	}

	// A third message, with no pending exercise left, falls through to
	// the correction path rather than being scored against a stale
	// exercise ID.
	thirdOut, err := h.svc.Handle(context.Background(), channels.Inbound{Channel: "slack", ExternalID: "U3", Text: "とても面白いでした", ThreadID: "t3"})
	if err != nil {
		t.Fatalf("Handle(third) returned error: %v", err)
	}
	if strings.HasPrefix(thirdOut.Text, "✓") || strings.HasPrefix(thirdOut.Text, "✗") {
		t.Fatalf("third reply = %q, want a correction reply, not another exercise verdict", thirdOut.Text)
	}
}

// (4) "help"/ヘルプ → a short command list, in either script.
func TestHandleHelpCommand(t *testing.T) {
	h := newTestHarness("slack:U4=learner-d")

	for _, text := range []string{"help", "HELP", "ヘルプ"} {
		out, err := h.svc.Handle(context.Background(), channels.Inbound{Channel: "slack", ExternalID: "U4", Text: text, ThreadID: "t4"})
		if err != nil {
			t.Fatalf("Handle(%q) returned error: %v", text, err)
		}
		if !strings.Contains(out.Text, "JLP commands") {
			t.Fatalf("Handle(%q) reply = %q, want a command list", text, out.Text)
		}
	}
	// help must not create a channel session or touch feedback/practice.
	if len(h.feedback.feedback) != 0 {
		t.Fatalf("persisted feedback count = %d, want 0 (help must not trigger a correction)", len(h.feedback.feedback))
	}
	if len(h.exercises.byID) != 0 {
		t.Fatalf("persisted exercises = %d, want 0 (help must not start practice)", len(h.exercises.byID))
	}
}

// (5) THE critical untrusted-edge pin: an unmapped sender gets a
// refusal, and — verified by counting every fake repository's contents,
// not by inspecting the reply text — NOTHING is written: no identity,
// no session, no document, no exercise, no event.
func TestHandleUnmappedSenderRefusesAndRecordsNothing(t *testing.T) {
	h := newTestHarness("slack:U1=learner-a") // U1 is mapped; "mallory" is not

	out, err := h.svc.Handle(context.Background(), channels.Inbound{
		Channel: "slack", ExternalID: "mallory", Text: "とても面白いでした", ThreadID: "t5",
	})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if out.Text == "" {
		t.Fatal("expected a non-empty refusal reply")
	}
	if strings.Contains(out.Text, "面白いでした") || strings.Contains(out.Text, "面白かったです") {
		t.Fatalf("refusal reply = %q, must not echo back the sender's own text or any correction", out.Text)
	}

	if len(h.identities.byID) != 0 {
		t.Fatalf("identities written = %d, want 0", len(h.identities.byID))
	}
	if len(h.sessions.sessions) != 0 {
		t.Fatalf("sessions written = %d, want 0", len(h.sessions.sessions))
	}
	if len(h.docs.docs) != 0 {
		t.Fatalf("documents written = %d, want 0", len(h.docs.docs))
	}
	if len(h.feedback.feedback) != 0 {
		t.Fatalf("feedback records written = %d, want 0", len(h.feedback.feedback))
	}
	if len(h.exercises.byID) != 0 {
		t.Fatalf("exercises written = %d, want 0", len(h.exercises.byID))
	}
	if len(h.events.events) != 0 {
		t.Fatalf("events recorded = %d, want 0", len(h.events.events))
	}
}

// (6) An unmapped sender sending "practice" or "help" still gets the
// same refusal, before the command is ever routed — the AllowFrom check
// must run first, unconditionally.
func TestHandleUnmappedSenderRefusesCommandsToo(t *testing.T) {
	h := newTestHarness("slack:U1=learner-a")

	for _, text := range []string{"practice", "help", "練習", "ヘルプ"} {
		out, err := h.svc.Handle(context.Background(), channels.Inbound{Channel: "slack", ExternalID: "mallory", Text: text, ThreadID: "t6"})
		if err != nil {
			t.Fatalf("Handle(%q) returned error: %v", text, err)
		}
		if strings.Contains(out.Text, "JLP commands") {
			t.Fatalf("Handle(%q) reply = %q, an unmapped sender must never see the help text", text, out.Text)
		}
	}
	if len(h.exercises.byID) != 0 {
		t.Fatalf("exercises written = %d, want 0", len(h.exercises.byID))
	}
}

func findSessionByTitle(repo *fakeSessionRepo, identity, title string) (session.Session, error) {
	for _, s := range repo.sessions {
		if string(s.IdentityID) == identity && s.Title == title {
			return s, nil
		}
	}
	return session.Session{}, errors.New("no session found with that title")
}
