//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// outcomeFixture is a per-test scratch world: its own pool, its own
// identities, and helpers that insert rows at EXACT timestamps.
// Timestamps have to be controlled precisely here — every count this
// repository returns is defined by a window boundary — and the
// application-layer services that normally write these tables all stamp
// now() themselves, so the seeding below goes straight to SQL.
type outcomeFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	repo *OutcomeRepository
}

func newOutcomeFixture(t *testing.T) *outcomeFixture {
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
	return &outcomeFixture{t: t, ctx: ctx, pool: pool, repo: NewOutcomeRepository(pool)}
}

func (f *outcomeFixture) identity(prefix string) learner.IdentityID {
	f.t.Helper()
	id := learner.IdentityID(prefix + "-" + uuid.NewString())
	if err := NewIdentityRepository(f.pool).Upsert(f.ctx, learner.Identity{ID: id, DisplayName: prefix}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *outcomeFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("seed %q: %v", sql, err)
	}
}

// concept registers a grammar_concepts catalog row. Slugs are suffixed
// per test run because grammar_concepts is a global catalog (no
// identity_id) shared with every other integration test in this package.
func (f *outcomeFixture) concept(slug, name string) string {
	f.t.Helper()
	unique := slug + "-" + uuid.NewString()[:8]
	f.exec(`INSERT INTO grammar_concepts (slug, name, jlpt_level) VALUES ($1, $2, 3)`, unique, name)
	return unique
}

func (f *outcomeFixture) session(identity learner.IdentityID) string {
	f.t.Helper()
	id := uuid.NewString()
	f.exec(`INSERT INTO sessions (id, identity_id, title) VALUES ($1, $2, 'outcome test')`, id, string(identity))
	return id
}

// correctionOn inserts one feedback request carrying selectionRunes
// characters of reviewed text plus one correction tagged with slug, all
// stamped at `at`. It returns the correction's replacement text so a
// caller can link a vocabulary expression to it.
func (f *outcomeFixture) correctionOn(identity learner.IdentityID, sessionID, slug, replacement string, selectionRunes int, at time.Time) {
	f.t.Helper()
	selection := ""
	for i := 0; i < selectionRunes; i++ {
		selection += "あ"
	}
	requestID := uuid.NewString()
	f.exec(`INSERT INTO feedback_requests
	          (id, identity_id, session_id, document_id, selection_start, selection_end, selection_text, created_at)
	        VALUES ($1, $2, $3, $4, 0, $5, $6, $7)`,
		requestID, string(identity), sessionID, f.document(identity, sessionID), selectionRunes, selection, at)
	correctionID := uuid.NewString()
	f.exec(`INSERT INTO corrections
	          (id, feedback_request_id, position, original, replacement, type, severity, created_at)
	        VALUES ($1, $2, 0, 'まちがい', $3, 'grammar', 'incorrect', $4)`,
		correctionID, requestID, replacement, at)
	f.exec(`INSERT INTO correction_concepts (correction_id, concept_slug, resolved) VALUES ($1, $2, true)`,
		correctionID, slug)
}

func (f *outcomeFixture) document(identity learner.IdentityID, sessionID string) string {
	f.t.Helper()
	id := uuid.NewString()
	// documents has a UNIQUE index on session_id, so a session reuses
	// its one document across every correction seeded against it.
	var existing string
	if err := f.pool.QueryRow(f.ctx, `SELECT id::text FROM documents WHERE session_id = $1`, sessionID).Scan(&existing); err == nil {
		return existing
	}
	f.exec(`INSERT INTO documents (id, session_id, identity_id, content) VALUES ($1, $2, $3, '')`,
		id, sessionID, string(identity))
	return id
}

func (f *outcomeFixture) learningEvent(identity learner.IdentityID, typ event.Type, subject, evidence string, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO learning_events (id, identity_id, type, subject, evidence, occurred_at)
	        VALUES ($1, $2, $3, $4, $5::jsonb, $6)`,
		uuid.NewString(), string(identity), string(typ), subject, evidence, at)
}

func (f *outcomeFixture) vocabItem(identity learner.IdentityID, expression string, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO vocabulary_items (id, identity_id, expression, first_seen, last_event)
	        VALUES ($1, $2, $3, $4, $4)`,
		uuid.NewString(), string(identity), expression, at)
}

func (f *outcomeFixture) ratedAttempt(identity learner.IdentityID, confidence int, correct bool, at time.Time) {
	f.t.Helper()
	exerciseID := uuid.NewString()
	f.exec(`INSERT INTO exercises (id, identity_id, type, payload, created_at)
	        VALUES ($1, $2, 'cloze', '{}'::jsonb, $3)`, exerciseID, string(identity), at)
	f.exec(`INSERT INTO exercise_attempts (id, exercise_id, response, correct, confidence, created_at)
	        VALUES ($1, $2, 'ans', $3, $4, $5)`, uuid.NewString(), exerciseID, correct, confidence, at)
}

func outcomeBySlug(t *testing.T, got []storage.ConceptOutcome, slug string) storage.ConceptOutcome {
	t.Helper()
	for _, c := range got {
		if c.Slug == slug {
			return c
		}
	}
	t.Fatalf("no outcome for %q in %+v", slug, got)
	return storage.ConceptOutcome{}
}

// TestConceptOutcomesCountsBothWindowsAndScopesToIdentity is the core
// window test: corrections inside the baseline window, corrections
// inside the recent window, and corrections in the gap between the two
// (which belong to neither count but still move TotalCorrections and
// LastCorrectedAt).
func TestConceptOutcomesCountsBothWindowsAndScopesToIdentity(t *testing.T) {
	f := newOutcomeFixture(t)
	identityA := f.identity("outcome-a")
	identityB := f.identity("outcome-b")
	sessionA := f.session(identityA)
	sessionB := f.session(identityB)
	slug := f.concept("te-form", "て形")

	now := time.Now().UTC()
	first := now.Add(-200 * 24 * time.Hour)

	// Three corrections inside the 30-day baseline window measured from
	// `first` (including `first` itself)...
	f.correctionOn(identityA, sessionA, slug, "食べて", 100, first)
	f.correctionOn(identityA, sessionA, slug, "書いて", 100, first.Add(5*24*time.Hour))
	f.correctionOn(identityA, sessionA, slug, "読んで", 100, first.Add(29*24*time.Hour))
	// ...one just outside it, in neither window...
	f.correctionOn(identityA, sessionA, slug, "泳いで", 100, first.Add(31*24*time.Hour))
	// ...and one inside the recent 30-day window.
	f.correctionOn(identityA, sessionA, slug, "走って", 100, now.Add(-2*24*time.Hour))

	// Identity B is corrected on the same concept, far more often, and
	// must never leak into A's counts.
	for i := 0; i < 7; i++ {
		f.correctionOn(identityB, sessionB, slug, "違って", 100, now.Add(-time.Duration(i)*24*time.Hour))
	}

	got, err := f.repo.ConceptOutcomes(context.Background(), identityA)
	if err != nil {
		t.Fatal(err)
	}
	oc := outcomeBySlug(t, got, slug)

	if oc.Name != "て形" {
		t.Errorf("Name = %q, want て形", oc.Name)
	}
	if oc.TotalCorrections != 5 {
		t.Errorf("TotalCorrections = %d, want 5", oc.TotalCorrections)
	}
	if oc.CorrectionsBefore != 3 {
		t.Errorf("CorrectionsBefore = %d, want 3", oc.CorrectionsBefore)
	}
	if oc.CorrectionsAfter != 1 {
		t.Errorf("CorrectionsAfter = %d, want 1", oc.CorrectionsAfter)
	}
	if d := oc.FirstCorrectedAt.Sub(first); d > time.Second || d < -time.Second {
		t.Errorf("FirstCorrectedAt = %v, want ~%v", oc.FirstCorrectedAt, first)
	}
	if !oc.LastCorrectedAt.After(now.Add(-3 * 24 * time.Hour)) {
		t.Errorf("LastCorrectedAt = %v, want the most recent correction", oc.LastCorrectedAt)
	}
	// The repository never judges — Retired is the analyser's job.
	if oc.Retired {
		t.Error("repository set Retired; that is a judgement the application layer owns")
	}
}

// TestConceptOutcomesExcludesUnresolvedTagsAndUncorrectedConcepts:
// a concept nobody was corrected on has no outcome to report, and a
// hallucinated (unresolved) tag must not create one.
func TestConceptOutcomesExcludesUnresolvedTagsAndUncorrectedConcepts(t *testing.T) {
	f := newOutcomeFixture(t)
	identity := f.identity("outcome-unresolved")
	session := f.session(identity)
	tagged := f.concept("passive", "受身")
	untouched := f.concept("causative", "使役")

	now := time.Now().UTC()
	f.correctionOn(identity, session, tagged, "食べられる", 50, now.Add(-10*24*time.Hour))

	// An unresolved tag on its own correction: present in the table,
	// must not produce an outcome row.
	unresolvedCorrection := uuid.NewString()
	requestID := uuid.NewString()
	f.exec(`INSERT INTO feedback_requests (id, identity_id, session_id, document_id, selection_start, selection_end, selection_text, created_at)
	        VALUES ($1, $2, $3, $4, 0, 3, 'あああ', $5)`,
		requestID, string(identity), session, f.document(identity, session), now)
	f.exec(`INSERT INTO corrections (id, feedback_request_id, position, original, replacement, type, severity, created_at)
	        VALUES ($1, $2, 0, 'x', 'y', 'grammar', 'incorrect', $3)`, unresolvedCorrection, requestID, now)
	f.exec(`INSERT INTO correction_concepts (correction_id, concept_slug, resolved) VALUES ($1, $2, false)`,
		unresolvedCorrection, untouched)

	got, err := f.repo.ConceptOutcomes(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Slug != tagged {
		t.Fatalf("ConceptOutcomes = %+v, want exactly the tagged concept", got)
	}
}

// TestConceptOutcomesWithNoCorrectionsAtAllIsEmpty pins the brand-new
// learner's path — an empty result, not a row full of zeroes.
func TestConceptOutcomesWithNoCorrectionsAtAllIsEmpty(t *testing.T) {
	f := newOutcomeFixture(t)
	got, err := f.repo.ConceptOutcomes(context.Background(), f.identity("outcome-fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ConceptOutcomes = %+v, want empty", got)
	}
}

// TestConceptOutcomesCountsIndependentSolvesOnly: only correction.retried
// events carrying independent=true count, and only for the concepts they
// actually name.
func TestConceptOutcomesCountsIndependentSolvesOnly(t *testing.T) {
	f := newOutcomeFixture(t)
	identity := f.identity("outcome-solves")
	other := f.identity("outcome-solves-other")
	session := f.session(identity)
	slug := f.concept("conditional", "条件形")
	otherSlug := f.concept("volitional", "意向形")

	now := time.Now().UTC()
	f.correctionOn(identity, session, slug, "行けば", 50, now.Add(-40*24*time.Hour))
	f.correctionOn(identity, session, otherSlug, "行こう", 50, now.Add(-40*24*time.Hour))

	f.learningEvent(identity, event.TypeCorrectionRetried, uuid.NewString(),
		`{"independent": true, "correct": true, "concepts": ["`+slug+`"]}`, now.Add(-3*24*time.Hour))
	f.learningEvent(identity, event.TypeCorrectionRetried, uuid.NewString(),
		`{"independent": true, "correct": true, "concepts": ["`+slug+`"]}`, now.Add(-2*24*time.Hour))
	// Correct but only after revealing the answer: not independent.
	f.learningEvent(identity, event.TypeCorrectionRetried, uuid.NewString(),
		`{"independent": false, "correct": true, "concepts": ["`+slug+`"]}`, now.Add(-time.Hour))
	// No concepts array at all (a producer that predates it): must not
	// error the query out.
	f.learningEvent(identity, event.TypeCorrectionRetried, uuid.NewString(),
		`{"independent": true, "correct": true}`, now.Add(-time.Hour))
	// Another identity's independent solve on the same concept.
	f.learningEvent(other, event.TypeCorrectionRetried, uuid.NewString(),
		`{"independent": true, "correct": true, "concepts": ["`+slug+`"]}`, now)

	got, err := f.repo.ConceptOutcomes(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if n := outcomeBySlug(t, got, slug).IndependentSolves; n != 2 {
		t.Errorf("IndependentSolves = %d, want 2", n)
	}
	if n := outcomeBySlug(t, got, otherSlug).IndependentSolves; n != 0 {
		t.Errorf("IndependentSolves for the untouched concept = %d, want 0", n)
	}
}

// TestConceptOutcomesCountsProducedCorrectlyForLinkedExpressions covers
// the containment link (expression inside a tagged correction's
// replacement), the after-the-correction ordering, and the
// count-once-per-event rule when several corrections carry the concept.
func TestConceptOutcomesCountsProducedCorrectlyForLinkedExpressions(t *testing.T) {
	f := newOutcomeFixture(t)
	identity := f.identity("outcome-produced")
	other := f.identity("outcome-produced-other")
	session := f.session(identity)
	otherSession := f.session(other)
	slug := f.concept("keigo", "敬語")

	now := time.Now().UTC()
	corrected := now.Add(-40 * 24 * time.Hour)
	// Two corrections on the same concept both containing 伺います: one
	// production event matching both must still count once.
	f.correctionOn(identity, session, slug, "明日伺います", 50, corrected)
	f.correctionOn(identity, session, slug, "伺いますので", 50, corrected.Add(time.Hour))

	f.vocabItem(identity, "伺います", corrected)
	f.vocabItem(identity, "食べます", corrected)

	// After the correction, linked: counts.
	f.learningEvent(identity, event.TypeVocabularyProducedCorrectly, "伺います", `{}`, now.Add(-5*24*time.Hour))
	// Before the correction: the correction cannot have caused it.
	f.learningEvent(identity, event.TypeVocabularyProducedCorrectly, "伺います", `{}`, corrected.Add(-24*time.Hour))
	// Not linked to any correction on this concept.
	f.learningEvent(identity, event.TypeVocabularyProducedCorrectly, "食べます", `{}`, now.Add(-time.Hour))
	// Produced, but NOT correctly.
	f.learningEvent(identity, event.TypeVocabularyProduced, "伺います", `{}`, now.Add(-time.Hour))

	// A SECOND identity, corrected on the same concept with the same
	// expression in the replacement, producing it correctly three times.
	// The query joins learning_events to vocabulary_items and to
	// feedback_requests on identity, so a dropped predicate on any of
	// those three would show up here as an inflated count for the first
	// identity — the reason every other method in this file has a
	// cross-identity case too.
	f.correctionOn(other, otherSession, slug, "明日伺います", 50, corrected)
	f.vocabItem(other, "伺います", corrected)
	for i := 0; i < 3; i++ {
		f.learningEvent(other, event.TypeVocabularyProducedCorrectly, "伺います", `{}`, now.Add(-time.Duration(i)*time.Hour))
	}

	// An empty-expression vocabulary row: position('' IN anything)
	// returns 1, so without the query's `vi.expression <> ''` guard this
	// single row would link EVERY correction to every production event.
	f.vocabItem(identity, "", corrected)

	got, err := f.repo.ConceptOutcomes(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if n := outcomeBySlug(t, got, slug).ProducedCorrectly; n != 1 {
		t.Errorf("ProducedCorrectly = %d, want 1", n)
	}

	otherGot, err := f.repo.ConceptOutcomes(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if n := outcomeBySlug(t, otherGot, slug).ProducedCorrectly; n != 3 {
		t.Errorf("second identity's ProducedCorrectly = %d, want 3", n)
	}
}

// TestCalibrationTrendZeroFillsAndScopesToIdentity: exactly `weeks`
// entries, oldest first, with untouched weeks explicit zeroes and the
// two ratios left for the application layer.
func TestCalibrationTrendZeroFillsAndScopesToIdentity(t *testing.T) {
	f := newOutcomeFixture(t)
	identity := f.identity("outcome-calib")
	other := f.identity("outcome-calib-other")

	now := time.Now().UTC()
	f.ratedAttempt(identity, 4, true, now)
	f.ratedAttempt(identity, 2, false, now)
	f.ratedAttempt(other, 5, true, now)
	// Unrated attempt (confidence NULL): never contributes.
	exerciseID := uuid.NewString()
	f.exec(`INSERT INTO exercises (id, identity_id, type, payload, created_at) VALUES ($1, $2, 'cloze', '{}'::jsonb, $3)`,
		exerciseID, string(identity), now)
	f.exec(`INSERT INTO exercise_attempts (id, exercise_id, response, correct, created_at) VALUES ($1, $2, 'x', true, $3)`,
		uuid.NewString(), exerciseID, now)

	got, err := f.repo.CalibrationTrend(context.Background(), identity, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("len = %d, want 8 zero-filled weeks", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].WeekStart.After(got[i-1].WeekStart) {
			t.Fatalf("weeks not ascending: %v", got)
		}
	}
	last := got[len(got)-1]
	if last.Attempts != 2 || last.ConfidenceTotal != 6 || last.Corrects != 1 {
		t.Errorf("current week = %+v, want 2 attempts / 6 confidence / 1 correct", last)
	}
	if last.MeanConfidence != 0 || last.CorrectRate != 0 {
		t.Errorf("repository derived ratios (%+v); that is the application layer's job", last)
	}
	for _, w := range got[:len(got)-1] {
		if w.Attempts != 0 {
			t.Errorf("week %v = %+v, want an explicit zero", w.WeekStart, w)
		}
	}
}

// TestAssistanceFadingCountsReviewedRunesOncePerRequest is the
// double-counting guard: a request with three corrections must
// contribute its selection length ONCE, not three times.
func TestAssistanceFadingCountsReviewedRunesOncePerRequest(t *testing.T) {
	f := newOutcomeFixture(t)
	identity := f.identity("outcome-fading")
	other := f.identity("outcome-fading-other")
	session := f.session(identity)
	otherSession := f.session(other)
	slug := f.concept("particle-wa", "は")

	now := time.Now().UTC()
	requestID := uuid.NewString()
	f.exec(`INSERT INTO feedback_requests (id, identity_id, session_id, document_id, selection_start, selection_end, selection_text, created_at)
	        VALUES ($1, $2, $3, $4, 0, 10, 'あいうえおかきくけこ', $5)`,
		requestID, string(identity), session, f.document(identity, session), now)
	for i := 0; i < 3; i++ {
		f.exec(`INSERT INTO corrections (id, feedback_request_id, position, original, replacement, type, severity, created_at)
		        VALUES ($1, $2, $3, 'x', 'y', 'grammar', 'incorrect', $4)`,
			uuid.NewString(), requestID, i, now)
	}
	// A second request the same week with no corrections at all: adds
	// runes but no corrections, which is exactly what fading looks like.
	f.exec(`INSERT INTO feedback_requests (id, identity_id, session_id, document_id, selection_start, selection_end, selection_text, created_at)
	        VALUES ($1, $2, $3, $4, 0, 5, 'さしすせそ', $5)`,
		uuid.NewString(), string(identity), session, f.document(identity, session), now)
	// Another identity's reviewed text must not leak in.
	f.correctionOn(other, otherSession, slug, "は", 999, now)

	got, err := f.repo.AssistanceFading(context.Background(), identity, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("len = %d, want 8 zero-filled weeks", len(got))
	}
	last := got[len(got)-1]
	if last.Runes != 15 {
		t.Errorf("Runes = %d, want 15 (10 + 5, each request counted once)", last.Runes)
	}
	if last.Corrections != 3 {
		t.Errorf("Corrections = %d, want 3", last.Corrections)
	}
	if last.Per1000 != 0 {
		t.Errorf("repository derived Per1000 = %v; that is the application layer's job", last.Per1000)
	}
	for _, w := range got[:len(got)-1] {
		if w.Runes != 0 || w.Corrections != 0 {
			t.Errorf("week %v = %+v, want an explicit zero", w.WeekStart, w)
		}
	}
}

// TestTrendsWithNonPositiveWeeksReturnNothing guards the zero-fill
// helpers against a caller asking for no window at all.
func TestTrendsWithNonPositiveWeeksReturnNothing(t *testing.T) {
	f := newOutcomeFixture(t)
	identity := f.identity("outcome-noweeks")
	for _, weeks := range []int{0, -1} {
		calib, err := f.repo.CalibrationTrend(context.Background(), identity, weeks)
		if err != nil || len(calib) != 0 {
			t.Errorf("CalibrationTrend(%d) = %v, %v", weeks, calib, err)
		}
		fading, err := f.repo.AssistanceFading(context.Background(), identity, weeks)
		if err != nil || len(fading) != 0 {
			t.Errorf("AssistanceFading(%d) = %v, %v", weeks, fading, err)
		}
	}
}
