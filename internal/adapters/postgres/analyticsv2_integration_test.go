//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// analyticsV2TestPool migrates and opens a pool once per test func — a
// small local helper so each Test below stays focused on its own
// seed/assert shape rather than repeating the boilerplate every
// existing postgres integration test already has.
func analyticsV2TestPool(t *testing.T) *AnalyticsRepository {
	t.Helper()
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewAnalyticsRepository(pool)
}

func newAnalyticsTestIdentity(t *testing.T, repo *AnalyticsRepository, prefix string) learner.Identity {
	t.Helper()
	// AnalyticsRepository doesn't expose the pool, so identities are
	// created through the same pool via a fresh IdentityRepository built
	// from testURL's connection — cheap, and keeps this helper
	// self-contained rather than threading a pool out of
	// analyticsV2TestPool just for this one call.
	ctx := context.Background()
	pool, err := NewPool(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	id := learner.Identity{ID: learner.IdentityID(prefix + "-" + uuid.NewString()), DisplayName: prefix}
	if err := NewIdentityRepository(pool).Upsert(ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestVocabFunnelScopedToIdentity covers VocabFunnel: sums lookups,
// productions, and successful_productions across every vocabulary item
// an identity owns, excluding another identity's rows entirely.
func TestVocabFunnelScopedToIdentity(t *testing.T) {
	ctx := context.Background()
	repo := analyticsV2TestPool(t)
	identityA := newAnalyticsTestIdentity(t, repo, "funnel-a")
	identityB := newAnalyticsTestIdentity(t, repo, "funnel-b")

	pool, err := NewPool(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	vocab := NewVocabularyRepository(pool)

	now := time.Now().UTC()
	itemA1, _, err := vocab.UpsertOnLookup(ctx, identityA.ID, "食べる", "たべる", "to eat", "app", "", vocabulary.KindWord, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := vocab.UpsertOnLookup(ctx, identityA.ID, "飲む", "のむ", "to drink", "app", "", vocabulary.KindWord, "", now); err != nil {
		t.Fatal(err)
	}
	if err := vocab.RecordProduction(ctx, identityA.ID, itemA1.ID, true, now); err != nil {
		t.Fatal(err)
	}
	if err := vocab.RecordProduction(ctx, identityA.ID, itemA1.ID, false, now); err != nil {
		t.Fatal(err)
	}

	// Identity B: a much larger funnel that must never leak into A's sums.
	itemB, _, err := vocab.UpsertOnLookup(ctx, identityB.ID, "走る", "はしる", "to run", "app", "", vocabulary.KindWord, "", now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := vocab.RecordProduction(ctx, identityB.ID, itemB.ID, true, now); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.VocabFunnel(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := storage.VocabFunnel{LookedUp: 2, Produced: 2, ProducedCorrectly: 1}
	if got != want {
		t.Fatalf("VocabFunnel(A) = %+v, want %+v", got, want)
	}

	identityC := newAnalyticsTestIdentity(t, repo, "funnel-c")
	gotC, err := repo.VocabFunnel(ctx, identityC.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotC != (storage.VocabFunnel{}) {
		t.Fatalf("VocabFunnel(no vocabulary) = %+v, want all zero", gotC)
	}
}

// TestWeaknessTrendsZeroFillsMissingWeeksAndScopesToIdentity covers
// both WeaknessTrends contracts the task brief calls out: a week with
// no qualifying learning_events for the subject must still appear as
// an explicit zero (never a gap), and results must never include
// another identity's weakness/events.
func TestWeaknessTrendsZeroFillsMissingWeeksAndScopesToIdentity(t *testing.T) {
	ctx := context.Background()
	repo := analyticsV2TestPool(t)
	identityA := newAnalyticsTestIdentity(t, repo, "trend-a")
	identityB := newAnalyticsTestIdentity(t, repo, "trend-b")

	pool, err := NewPool(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	obs := NewObservationRepository(pool)
	events := NewLearningEventRepository(pool)

	now := time.Now().UTC()
	const subject = "i-adjective-past"
	if err := obs.Upsert(ctx, learnermodel.Observation{
		ID: uuid.NewString(), IdentityID: identityA.ID, Kind: learnermodel.KindWeakness,
		SubjectType: learnermodel.SubjectConcept, Subject: subject, Confidence: 1.0,
		FirstSeen: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Two occurrences THIS week, none in the prior 7 weeks: every other
	// bucket must come back as an explicit 0, not be missing.
	for i := 0; i < 2; i++ {
		if err := events.Append(ctx, event.LearningEvent{
			ID: uuid.NewString(), IdentityID: identityA.ID, Type: event.TypeGrammarConceptEncountered,
			Subject: subject, Evidence: map[string]any{}, OccurredAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A second, correction-type weakness for A: exercises the SQL
	// query's other branch, where the subject text lives in
	// evidence->>'type' rather than the learning_events.subject column
	// (which holds the correction ID for these — see
	// SubjectOccurrencesByWeek's doc comment).
	const correctionSubject = "particle"
	if err := obs.Upsert(ctx, learnermodel.Observation{
		ID: uuid.NewString(), IdentityID: identityA.ID, Kind: learnermodel.KindWeakness,
		SubjectType: learnermodel.SubjectCorrectionType, Subject: correctionSubject, Confidence: 0.6,
		FirstSeen: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := events.Append(ctx, event.LearningEvent{
			ID: uuid.NewString(), IdentityID: identityA.ID, Type: event.TypeCorrectionPresented,
			Subject: uuid.NewString(), // the correction ID, NOT the type text.
			Evidence: map[string]any{"type": correctionSubject}, OccurredAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Identity B: same subject text, own weakness + events — must never
	// leak into A's trend.
	if err := obs.Upsert(ctx, learnermodel.Observation{
		ID: uuid.NewString(), IdentityID: identityB.ID, Kind: learnermodel.KindWeakness,
		SubjectType: learnermodel.SubjectConcept, Subject: subject, Confidence: 1.0,
		FirstSeen: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		if err := events.Append(ctx, event.LearningEvent{
			ID: uuid.NewString(), IdentityID: identityB.ID, Type: event.TypeGrammarConceptEncountered,
			Subject: subject, Evidence: map[string]any{}, OccurredAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.WeaknessTrends(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("WeaknessTrends(A) = %d subjects, want 2: %+v", len(got), got)
	}
	bySubject := make(map[string]storage.SubjectTrend, len(got))
	for _, trend := range got {
		bySubject[trend.Subject] = trend
	}

	trend, ok := bySubject[subject]
	if !ok {
		t.Fatalf("WeaknessTrends(A) missing concept subject %q: %+v", subject, got)
	}
	if len(trend.Weeks) != 8 {
		t.Fatalf("len(trend.Weeks) = %d, want 8 (fixed window, zero-filled)", len(trend.Weeks))
	}
	total := 0
	for i, w := range trend.Weeks {
		total += w.Count
		if i < 7 && w.Count != 0 {
			t.Errorf("trend.Weeks[%d].Count = %d, want 0 (zero-filled, no occurrences that week)", i, w.Count)
		}
	}
	if trend.Weeks[7].Count != 2 {
		t.Errorf("trend.Weeks[7] (current week).Count = %d, want 2", trend.Weeks[7].Count)
	}
	if total != 2 {
		t.Errorf("total occurrences across all weeks = %d, want 2 (must exclude identity B's 9)", total)
	}

	correctionTrend, ok := bySubject[correctionSubject]
	if !ok {
		t.Fatalf("WeaknessTrends(A) missing correction-type subject %q: %+v", correctionSubject, got)
	}
	correctionTotal := 0
	for _, w := range correctionTrend.Weeks {
		correctionTotal += w.Count
	}
	if correctionTotal != 3 {
		t.Errorf("correction-type trend total = %d, want 3", correctionTotal)
	}
	if correctionTrend.Weeks[7].Count != 3 {
		t.Errorf("correction-type trend.Weeks[7] (current week).Count = %d, want 3", correctionTrend.Weeks[7].Count)
	}
}

// TestConfidenceCalibrationOnlyRatedAttemptsAndEmptyIsExplicit covers
// ConfidenceCalibration: attempts with no confidence value never
// contribute a row, only observed confidence levels appear, an
// identity with zero rated attempts gets an empty (not nil-panicking)
// slice back, and — matching TestVocabFunnelScopedToIdentity's
// rigour — a second identity's OVERLAPPING confidence values (same
// confidence levels, different attempt/correct counts) never leak
// into identity A's rows. The repository leaves CorrectRate at its
// zero value throughout (see storage.ConfidenceCalibration's doc
// comment) — application/analytics.Service derives it, covered by
// that package's own unit tests, not here.
func TestConfidenceCalibrationOnlyRatedAttemptsAndEmptyIsExplicit(t *testing.T) {
	ctx := context.Background()
	repo := analyticsV2TestPool(t)
	identityA := newAnalyticsTestIdentity(t, repo, "calib-a")
	identityB := newAnalyticsTestIdentity(t, repo, "calib-b")
	identityEmpty := newAnalyticsTestIdentity(t, repo, "calib-empty")

	pool, err := NewPool(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exercises := NewExerciseRepository(pool)

	now := time.Now().UTC()
	mkExercise := func(identity learner.IdentityID) exercise.Exercise {
		ex := exercise.Exercise{
			ID: uuid.NewString(), IdentityID: identity, ConceptSlug: "i-adjective-past", Type: "fill-in-blank",
			InstructionsJA: "埋めてください", Prompt: "prompt", Answer: "answer", CreatedAt: now,
		}
		if err := exercises.Create(ctx, ex); err != nil {
			t.Fatal(err)
		}
		return ex
	}
	recordAttempt := func(exID string, correct bool, confidence *int) {
		if err := exercises.RecordAttempt(ctx, storage.ExerciseAttempt{
			ID: uuid.NewString(), ExerciseID: exID, Response: "resp", Correct: correct,
			Confidence: confidence, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	conf := func(v int) *int { return &v }

	// confidence=4: 2 attempts, 1 correct.
	ex1, ex2 := mkExercise(identityA.ID), mkExercise(identityA.ID)
	recordAttempt(ex1.ID, true, conf(4))
	recordAttempt(ex2.ID, false, conf(4))
	// confidence=2: 1 attempt, correct.
	ex3 := mkExercise(identityA.ID)
	recordAttempt(ex3.ID, true, conf(2))
	// No confidence given at all: must never surface as a row.
	ex4 := mkExercise(identityA.ID)
	recordAttempt(ex4.ID, true, nil)

	// Identity B: the SAME two confidence levels (4 and 2) as A, but a
	// larger, all-correct set of attempts — if scoping ever broke, B's
	// rows would inflate A's Attempts/Corrects rather than merely
	// appearing as separate rows, since both identities use the same
	// confidence keys.
	exB1, exB2, exB3 := mkExercise(identityB.ID), mkExercise(identityB.ID), mkExercise(identityB.ID)
	recordAttempt(exB1.ID, true, conf(4))
	recordAttempt(exB2.ID, true, conf(4))
	recordAttempt(exB3.ID, true, conf(2))

	got, err := repo.ConfidenceCalibration(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []storage.ConfidenceCalibration{
		{Confidence: 2, Attempts: 1, Corrects: 1},
		{Confidence: 4, Attempts: 2, Corrects: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("ConfidenceCalibration(A) = %+v, want %+v (must exclude identity B's overlapping rows)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ConfidenceCalibration(A)[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	gotB, err := repo.ConfidenceCalibration(ctx, identityB.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantB := []storage.ConfidenceCalibration{
		{Confidence: 2, Attempts: 1, Corrects: 1},
		{Confidence: 4, Attempts: 2, Corrects: 2},
	}
	if len(gotB) != len(wantB) {
		t.Fatalf("ConfidenceCalibration(B) = %+v, want %+v (must exclude identity A's overlapping rows)", gotB, wantB)
	}
	for i := range wantB {
		if gotB[i] != wantB[i] {
			t.Errorf("ConfidenceCalibration(B)[%d] = %+v, want %+v", i, gotB[i], wantB[i])
		}
	}

	gotEmpty, err := repo.ConfidenceCalibration(ctx, identityEmpty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotEmpty) != 0 {
		t.Fatalf("ConfidenceCalibration(no rated attempts) = %+v, want empty", gotEmpty)
	}
}

// TestAgentUsageGroupsByAgentIncludingBlankAndScopesToIdentity covers
// AgentUsage: grouped per agent (including the '' bucket predating the
// 00017 migration), raw success count and latency computed correctly,
// and identity-scoped. The repository leaves SuccessRate at its zero
// value (see storage.AgentUsage's doc comment) — asserted explicitly
// below, mirroring analytics_test.go's "repository must not compute
// derived ratios" assertion for Statistics.
func TestAgentUsageGroupsByAgentIncludingBlankAndScopesToIdentity(t *testing.T) {
	ctx := context.Background()
	repo := analyticsV2TestPool(t)
	identityA := newAnalyticsTestIdentity(t, repo, "agent-a")
	identityB := newAnalyticsTestIdentity(t, repo, "agent-b")

	pool, err := NewPool(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reqs := NewAIRequestRepository(pool)

	now := time.Now().UTC()
	insert := func(identity learner.IdentityID, agent string, success bool, latencyMS int) {
		if err := reqs.Insert(ctx, storage.AIRequestRecord{
			ID: uuid.NewString(), IdentityID: identity, Capability: "structured-generation",
			Provider: "fake", Model: "fake-1", PromptName: "x", PromptVersion: "v1",
			LatencyMS: latencyMS, Success: success, CreatedAt: now, Agent: agent,
		}); err != nil {
			t.Fatal(err)
		}
	}

	insert(identityA.ID, "teacher", true, 100)
	insert(identityA.ID, "teacher", false, 200)
	insert(identityA.ID, "", true, 50) // pre-migration row: blank agent.

	// Identity B: must never leak into A's grouping.
	insert(identityB.ID, "teacher", true, 999)

	got, err := repo.AgentUsage(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	byAgent := make(map[string]storage.AgentUsage, len(got))
	for _, row := range got {
		byAgent[row.Agent] = row
	}
	if len(got) != 2 {
		t.Fatalf("AgentUsage(A) = %+v, want 2 groups (teacher, blank)", got)
	}
	teacher, ok := byAgent["teacher"]
	if !ok {
		t.Fatalf("AgentUsage(A) missing teacher group: %+v", got)
	}
	if teacher.Requests != 2 || teacher.Successes != 1 || teacher.AvgLatencyMS != 150 {
		t.Errorf("teacher group = %+v, want {Requests:2 Successes:1 AvgLatencyMS:150}", teacher)
	}
	if teacher.SuccessRate != 0 {
		t.Errorf("teacher group SuccessRate = %v, want 0 (repository must not compute derived ratios)", teacher.SuccessRate)
	}
	blank, ok := byAgent[""]
	if !ok {
		t.Fatalf("AgentUsage(A) missing blank-agent group: %+v", got)
	}
	if blank.Requests != 1 || blank.Successes != 1 || blank.AvgLatencyMS != 50 {
		t.Errorf("blank group = %+v, want {Requests:1 Successes:1 AvgLatencyMS:50}", blank)
	}
}

// TestSystemStatsScopedToIdentity covers SystemStats: LearningEvents
// equals the sum of EventsByType, AIRequests counts only identity's own
// ai_requests rows, and everything excludes another identity entirely.
func TestSystemStatsScopedToIdentity(t *testing.T) {
	ctx := context.Background()
	repo := analyticsV2TestPool(t)
	identityA := newAnalyticsTestIdentity(t, repo, "sys-a")
	identityB := newAnalyticsTestIdentity(t, repo, "sys-b")

	pool, err := NewPool(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	events := NewLearningEventRepository(pool)
	reqs := NewAIRequestRepository(pool)

	now := time.Now().UTC()
	appendEvent := func(identity learner.IdentityID, typ event.Type) {
		if err := events.Append(ctx, event.LearningEvent{
			ID: uuid.NewString(), IdentityID: identity, Type: typ, Evidence: map[string]any{}, OccurredAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(identityA.ID, event.TypeCorrectionPresented)
	appendEvent(identityA.ID, event.TypeCorrectionPresented)
	appendEvent(identityA.ID, event.TypeGrammarConceptEncountered)
	appendEvent(identityB.ID, event.TypeCorrectionPresented) // must not leak into A.

	if err := reqs.Insert(ctx, storage.AIRequestRecord{
		ID: uuid.NewString(), IdentityID: identityA.ID, Capability: "structured-generation",
		Provider: "fake", Model: "fake-1", PromptName: "x", PromptVersion: "v1", Success: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reqs.Insert(ctx, storage.AIRequestRecord{
		ID: uuid.NewString(), IdentityID: identityB.ID, Capability: "structured-generation",
		Provider: "fake", Model: "fake-1", PromptName: "x", PromptVersion: "v1", Success: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.SystemStats(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LearningEvents != 3 {
		t.Errorf("SystemStats(A).LearningEvents = %d, want 3 (must exclude identity B's 1)", got.LearningEvents)
	}
	if got.EventsByType["correction.presented"] != 2 || got.EventsByType["grammar.concept.encountered"] != 1 {
		t.Errorf("SystemStats(A).EventsByType = %+v, want {correction.presented:2 grammar.concept.encountered:1}", got.EventsByType)
	}
	if got.AIRequests != 1 {
		t.Errorf("SystemStats(A).AIRequests = %d, want 1 (must exclude identity B's 1)", got.AIRequests)
	}
}
