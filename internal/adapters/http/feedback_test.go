package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeGrammarRepo is a minimal storage.GrammarRepository double for the
// HTTP-layer tests. ListConcepts returns the same two slugs fakeai's
// deterministic rules actually tag (see
// internal/application/feedback/service_test.go's
// knownConceptsForFakeAI, which this mirrors) so a correction's tag
// resolves the same way it would against the real catalog — needed for
// TestCorrectionStatusAcceptedKeepsConceptChip below, which checks a
// resolved concept chip survives an accept. Every other method panics
// if ever called: these tests don't exercise the rest of the catalog
// surface.
type fakeGrammarRepo struct{}

func (fakeGrammarRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by feedback http tests")
}

func (fakeGrammarRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	return []grammar.Concept{
		{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5},
		{Slug: "particle-ni-direction", Name: "に (direction/target/time)", JLPTLevel: 5},
	}, nil
}

func (fakeGrammarRepo) GetConcept(context.Context, string) (grammar.Concept, error) {
	panic("not used by feedback http tests")
}

func (fakeGrammarRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	panic("not used by feedback http tests")
}

func (fakeGrammarRepo) CorrectionsForConcept(context.Context, learner.IdentityID, string, int) ([]storage.CorrectionRecord, error) {
	panic("not used by feedback http tests")
}

// fakePriorityRepo is a minimal storage.PriorityRepository double for
// the feedback HTTP-layer tests: they exercise the review pipeline, not
// the planner, so Top always answers empty (no priorities seeded) and
// ReplaceAll is never expected to be called from this path.
type fakePriorityRepo struct{}

func (fakePriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by feedback http tests")
}

func (fakePriorityRepo) Top(context.Context, learner.IdentityID, int) ([]storage.Priority, error) {
	return nil, nil
}

// fakeFeedbackRepo is an in-memory storage.FeedbackRepository for
// HTTP-layer tests, mirroring the identity-scoped join
// application/feedback/service_test.go's double performs: a
// correction's identity is only known via the feedback record it
// belongs to.
type fakeFeedbackRepo struct {
	feedback    map[string]storage.FeedbackRecord
	corrections map[string]storage.CorrectionRecord
	// concepts maps correction ID -> concept slug -> resolved, tracking
	// every concept tag InsertFeedback was given so GetCorrectionConcepts
	// can answer for real (needed for TestCorrectionStatusAcceptedKeepsConceptChip).
	concepts map[string]map[string]bool
}

func newFakeFeedbackRepo() *fakeFeedbackRepo {
	return &fakeFeedbackRepo{
		feedback:    map[string]storage.FeedbackRecord{},
		corrections: map[string]storage.CorrectionRecord{},
		concepts:    map[string]map[string]bool{},
	}
}

// InsertFeedback mirrors the real repository's single-transaction
// contract: rec, corrections, and concepts (keyed by correction ID) are
// written together.
func (f *fakeFeedbackRepo) InsertFeedback(_ context.Context, rec storage.FeedbackRecord, corrections []storage.CorrectionRecord, concepts map[string][]storage.ConceptTag) error {
	f.feedback[rec.ID] = rec
	for _, c := range corrections {
		f.corrections[c.ID] = c
	}
	for correctionID, tags := range concepts {
		if f.concepts[correctionID] == nil {
			f.concepts[correctionID] = map[string]bool{}
		}
		for _, tag := range tags {
			f.concepts[correctionID][tag.Slug] = tag.Resolved
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
// RevealCorrection WHERE clause — see the identically-named helper in
// application/feedback/service_test.go, which this is a copy of.
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

// GetCorrectionConcepts mirrors the real query's "resolved only,
// slug-ascending" contract.
func (f *fakeFeedbackRepo) GetCorrectionConcepts(_ context.Context, correctionID string) ([]string, error) {
	var out []string
	for slug, resolved := range f.concepts[correctionID] {
		if resolved {
			out = append(out, slug)
		}
	}
	sort.Strings(out)
	return out, nil
}

// feedbackTestServer wires a real chi router with a real
// feedback.Service (a real teacher.Agent over fakeai — deterministic,
// no network) over in-memory repos, the same "real collaborators, fake
// edges" shape the rest of the HTTP-layer tests use. It creates a
// session, opens its document, and autosaves content into it, then
// returns the handler plus the session and document id the test posts
// against. TeacherMode is fixed "teacher" — see
// socraticFeedbackTestServer for Task 8's socratic-mode variant.
func feedbackTestServer(t *testing.T, content string) (http.Handler, session.Session, string) {
	t.Helper()
	h, sess, docID, _ := feedbackTestServerWithMode(t, content, "teacher")
	return h, sess, docID
}

// socraticFeedbackTestServer is feedbackTestServer with TeacherMode
// "socratic" — the mode adapters/fakeai keys hint attachment off of, so
// a review of the known-bad conjugation comes back with a hint (Phase 2
// Task 8, PRD §9/§53). It also returns the fake event repo (unlike
// feedbackTestServer) so retry/reveal/confidence tests can assert on
// hint.shown/correction.retried/answer.revealed/confidence.recorded
// Evidence directly, not just the rendered card.
func socraticFeedbackTestServer(t *testing.T, content string) (http.Handler, session.Session, string, *fakeEventRepo) {
	t.Helper()
	return feedbackTestServerWithMode(t, content, "socratic")
}

func feedbackTestServerWithMode(t *testing.T, content, teacherMode string) (http.Handler, session.Session, string, *fakeEventRepo) {
	t.Helper()
	opts := testOptions()
	sessionRepo := newFakeSessionRepo()
	docRepo := newFakeDocRepo()
	feedbackRepo := newFakeFeedbackRepo()
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())

	opts.Sessions = sessions.NewService(sessionRepo)
	opts.Writing = appwriting.NewService(docRepo, rec)
	opts.Events = events
	vocabRepo := newFakeVocabRepo()
	vocabSvc := appvocabulary.NewService(vocabRepo, rec)
	// teachingPlanner is real (not a fake): RequestFeedback calls its
	// ActivationCandidates directly (see application/feedback.Service's
	// NewService doc comment). obsRepo is only there to satisfy
	// NewPlanner's signature — ActivationCandidates never touches it.
	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, fakeGrammarRepo{}, fakePriorityRepo{}, vocabRepo, time.Now)
	opts.Feedback = appfeedback.NewService(sessionRepo, docRepo, feedbackRepo, fakeGrammarRepo{}, fakePriorityRepo{}, teachingPlanner, vocabSvc, teacher.New(fakeai.New()), rec)

	sess, err := opts.Sessions.Create(context.Background(), "dev", "日記", "Diary", session.Profile{
		TeacherMode:         teacherMode,
		Strictness:          "balanced",
		ExplanationLanguage: "both",
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := opts.Writing.Open(context.Background(), "dev", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err = opts.Writing.Autosave(context.Background(), "dev", doc.ID, content)
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(opts)
	return srv.HandlerForTest(), sess, string(doc.ID), events
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// extractCorrectionID pulls the id out of the first `id="corr-XXXX"`
// attribute in body, the way a browser's DOM would let a click handler
// find it.
func extractCorrectionID(t *testing.T, body string) string {
	t.Helper()
	const marker = `id="corr-`
	idx := strings.Index(body, marker)
	if idx == -1 {
		t.Fatalf("body missing correction id marker %q: %s", marker, body)
	}
	rest := body[idx+len(marker):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatalf("malformed correction id attribute: %s", body)
	}
	return rest[:end]
}

// TestFeedbackRequestThenAcceptCorrection is the core loop end to end:
// POST feedback on a selection containing the known-bad conjugation
// (see internal/adapters/fakeai) returns a rendered correction with
// the diff and both explanations, and posting "accepted" on that
// correction's id flips its rendered status.
func TestFeedbackRequestThenAcceptCorrection(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	form.Set("text", content)
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "面白かったです") {
		t.Fatalf("body missing corrected text 面白かったです: %s", body)
	}
	if !strings.Contains(body, "d-ins") {
		t.Fatalf("body missing d-ins diff span: %s", body)
	}
	if !strings.Contains(body, "d-del") {
		t.Fatalf("body missing d-del diff span: %s", body)
	}
	wantJA := "い形容詞の過去形は「〜かった」を使います。「面白い」→「面白かった」。"
	wantEN := "い-adjectives form the past tense with 〜かった, so 面白いでした must be 面白かったです."
	if !strings.Contains(body, wantJA) {
		t.Fatalf("body missing Japanese explanation %q: %s", wantJA, body)
	}
	if !strings.Contains(body, wantEN) {
		t.Fatalf("body missing English explanation %q: %s", wantEN, body)
	}
	if !strings.Contains(body, "conjugation") {
		t.Fatalf("body missing type badge conjugation: %s", body)
	}
	if !strings.Contains(body, "incorrect") {
		t.Fatalf("body missing severity badge incorrect: %s", body)
	}

	correctionID := extractCorrectionID(t, body)

	statusForm := url.Values{}
	statusForm.Set("status", "accepted")
	statusRec := postForm(t, h, "/corrections/"+correctionID+"/status", statusForm)

	if statusRec.Code != http.StatusOK {
		t.Fatalf("POST correction status = %d, body=%s", statusRec.Code, statusRec.Body.String())
	}
	if !strings.Contains(statusRec.Body.String(), `data-status="accepted"`) {
		t.Fatalf("body missing data-status=\"accepted\": %s", statusRec.Body.String())
	}
	if strings.Contains(statusRec.Body.String(), "納得した") {
		t.Fatalf("accepted card should not still show the accept button: %s", statusRec.Body.String())
	}
}

// TestCorrectionStatusAcceptedKeepsConceptChip pins the code-review fix
// for correctionViewFromRecord silently dropping concept chips:
// SetCorrectionStatus's re-rendered card (after accept/reject) must
// still carry the concept chip a tagged correction showed on its
// initial render, fetched back via GetCorrectionConcepts since
// storage.CorrectionRecord itself carries no Concepts field.
func TestCorrectionStatusAcceptedKeepsConceptChip(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	form.Set("text", content)
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/grammar/i-adjective-past"`) {
		t.Fatalf("initial card missing the i-adjective-past chip: %s", body)
	}

	correctionID := extractCorrectionID(t, body)

	statusForm := url.Values{}
	statusForm.Set("status", "accepted")
	statusRec := postForm(t, h, "/corrections/"+correctionID+"/status", statusForm)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("POST correction status = %d, body=%s", statusRec.Code, statusRec.Body.String())
	}
	statusBody := statusRec.Body.String()
	if !strings.Contains(statusBody, `data-status="accepted"`) {
		t.Fatalf("body missing data-status=\"accepted\": %s", statusBody)
	}
	if !strings.Contains(statusBody, `href="/grammar/i-adjective-past"`) {
		t.Fatalf("accepted card lost its i-adjective-past chip: %s", statusBody)
	}
}

// TestFeedbackRequestEmptySelectionReviewsWholeDocument mirrors the
// "collapse selection, review whole document" browser flow: start==end
// finds the same correction.
func TestFeedbackRequestEmptySelectionReviewsWholeDocument(t *testing.T) {
	content := "昨日友達と映画を見に行って、とても面白いでした。"
	h, sess, docID := feedbackTestServer(t, content)

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", "0")
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "面白かったです") {
		t.Fatalf("whole-document review missing corrected text: %s", rec.Body.String())
	}
}

// TestFeedbackRequestCrossIdentitySessionNotFound: a session owned by
// another identity must 404, not leak feedback.
func TestFeedbackRequestCrossIdentitySessionNotFound(t *testing.T) {
	h, _, docID := feedbackTestServer(t, "とても面白いでした")

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", "1")
	rec := postForm(t, h, "/sessions/someone-elses-session/feedback", form)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestFeedbackRequestBadStartReturnsBadRequest: a non-integer start
// field is a client error, not a 500.
func TestFeedbackRequestBadStartReturnsBadRequest(t *testing.T) {
	h, sess, docID := feedbackTestServer(t, "とても面白いでした")

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "not-a-number")
	form.Set("end", "1")
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// --- Phase 2 Task 8: active recall (hints, retry, reveal) + confidence
// tracking (PRD §9/§53). ---

// socraticRoundTrip posts feedback against a fresh socratic session and
// returns the rendered card body plus the correction ID extracted from
// it, plus the events repo — the setup every test below builds on.
func socraticRoundTrip(t *testing.T, content string) (h http.Handler, body, correctionID string, events *fakeEventRepo) {
	t.Helper()
	h, sess, docID, events := socraticFeedbackTestServer(t, content)
	runeLen := len([]rune(content))

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	form.Set("text", content)
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	return h, body, extractCorrectionID(t, body), events
}

// TestSocraticFeedbackHidesAnswerShowsHint pins the brief's Step 4
// "feedback → card shows the HINT, no 面白かったです visible" scenario:
// the initial card renders the hint and a retry/reveal affordance, but
// neither the corrected form (bare, nor inside the diff, nor inside the
// English/Japanese explanation prose) appears anywhere in the body.
func TestSocraticFeedbackHidesAnswerShowsHint(t *testing.T) {
	_, body, correctionID, events := socraticRoundTrip(t, "とても面白いでした")

	if correctionID == "" {
		t.Fatal("no correction id found in body")
	}
	if !strings.Contains(body, "い形容詞の過去形の作り方を思い出してください") {
		t.Fatalf("body missing the JA hint: %s", body)
	}
	if !strings.Contains(body, "Recall how い-adjectives form the past tense") {
		t.Fatalf("body missing the EN hint: %s", body)
	}
	if strings.Contains(body, "面白かったです") {
		t.Fatalf("body leaks the corrected form 面白かったです pre-reveal: %s", body)
	}
	// The bare replacement text (かったです, the diff's own insert span
	// content — see toDiffSpans) must not appear either: a whole-
	// selection diff (rendered by the "feedback" partial ABOVE the
	// individual card, from fb.Diff, not this card's own gated diff)
	// segments finer than "面白かったです" as one unit, so checking only
	// for the full contiguous replacement string would miss a diff-block
	// leak whose d-ins span happens to start after the shared "面白"
	// prefix — this pins the fix for exactly that gap (Step 4's browser
	// verification originally caught it; this codifies it).
	if strings.Contains(body, "d-ins") {
		t.Fatalf("body contains a d-ins diff span pre-reveal (leaks the answer via the whole-selection diff block): %s", body)
	}
	if !strings.Contains(body, `name="attempt"`) {
		t.Fatalf("body missing the retry input: %s", body)
	}
	if !strings.Contains(body, "答えを見る") {
		t.Fatalf("body missing the reveal button: %s", body)
	}
	if strings.Contains(body, "納得した") {
		t.Fatalf("socratic pre-reveal card should not show the plain accept button: %s", body)
	}

	var found bool
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == event.TypeHintShown && ev.Subject == correctionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a hint.shown event for %s, found none: %+v", correctionID, events.byIdentity["dev"])
	}
}

// TestCorrectionRetryWrongAttemptShowsAttemptsStillHidesAnswer pins
// "wrong retry → attempts 1, still hidden".
func TestCorrectionRetryWrongAttemptShowsAttemptsStillHidesAnswer(t *testing.T) {
	h, _, correctionID, _ := socraticRoundTrip(t, "とても面白いでした")

	form := url.Values{}
	form.Set("attempt", "面白いです")
	rec := postForm(t, h, "/corrections/"+correctionID+"/retry", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST retry status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "試行回数: 1") {
		t.Fatalf("body missing attempts count: %s", body)
	}
	if strings.Contains(body, "面白かったです") {
		t.Fatalf("body leaks the corrected form after a wrong retry: %s", body)
	}
	if !strings.Contains(body, `data-status="presented"`) {
		t.Fatalf("body status should remain presented after a wrong retry: %s", body)
	}
	if strings.Contains(body, "正解") {
		t.Fatalf("body should not show the correct-banner after a wrong retry: %s", body)
	}
}

// TestCorrectionRetryCorrectShowsBannerAcceptedAndConfidenceWidget pins
// "correct retry → 正解 banner + accepted + confidence stars", and that
// correction.retried recorded independent=true (never revealed).
func TestCorrectionRetryCorrectShowsBannerAcceptedAndConfidenceWidget(t *testing.T) {
	h, _, correctionID, events := socraticRoundTrip(t, "とても面白いでした")

	form := url.Values{}
	form.Set("attempt", "面白かったです")
	rec := postForm(t, h, "/corrections/"+correctionID+"/retry", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST retry status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "正解！") {
		t.Fatalf("body missing the 正解！ banner: %s", body)
	}
	if !strings.Contains(body, `data-status="accepted"`) {
		t.Fatalf("body missing data-status=\"accepted\": %s", body)
	}
	if !strings.Contains(body, "面白かったです") {
		t.Fatalf("body should show the corrected form once accepted: %s", body)
	}
	if !strings.Contains(body, "どのくらい自信がありましたか") {
		t.Fatalf("body missing the confidence widget: %s", body)
	}

	var last event.LearningEvent
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == event.TypeCorrectionRetried {
			last = ev
		}
	}
	if last.Type != event.TypeCorrectionRetried {
		t.Fatal("no correction.retried event recorded")
	}
	if got := last.Evidence["independent"]; got != true {
		t.Fatalf("Evidence[independent] = %v, want true", got)
	}
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == event.TypeCorrectionAccepted {
			t.Fatalf("unexpectedly recorded a separate correction.accepted event: %+v", ev)
		}
	}
}

// TestCorrectionRevealShowsAnswerThenRetryRecordsIndependentFalse pins
// the brief's Step 4 second-round scenario: 答えを見る reveals the
// answer (still Status "presented"), and a SUBSEQUENT correct retry
// records independent=false in correction.retried's Evidence.
func TestCorrectionRevealShowsAnswerThenRetryRecordsIndependentFalse(t *testing.T) {
	h, _, correctionID, events := socraticRoundTrip(t, "とても面白いでした")

	revealRec := postForm(t, h, "/corrections/"+correctionID+"/reveal", url.Values{})
	if revealRec.Code != http.StatusOK {
		t.Fatalf("POST reveal status = %d, body=%s", revealRec.Code, revealRec.Body.String())
	}
	revealBody := revealRec.Body.String()
	if !strings.Contains(revealBody, "面白かったです") {
		t.Fatalf("body should show the answer after reveal: %s", revealBody)
	}
	if !strings.Contains(revealBody, `data-status="presented"`) {
		t.Fatalf("reveal alone should not resolve the correction: %s", revealBody)
	}
	var revealed bool
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == event.TypeAnswerRevealed && ev.Subject == correctionID {
			revealed = true
		}
	}
	if !revealed {
		t.Fatalf("expected an answer.revealed event, found none: %+v", events.byIdentity["dev"])
	}

	form := url.Values{}
	form.Set("attempt", "面白かったです")
	retryRec := postForm(t, h, "/corrections/"+correctionID+"/retry", form)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("POST retry (after reveal) status = %d, body=%s", retryRec.Code, retryRec.Body.String())
	}

	var last event.LearningEvent
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == event.TypeCorrectionRetried {
			last = ev
		}
	}
	if last.Type != event.TypeCorrectionRetried {
		t.Fatal("no correction.retried event recorded")
	}
	if got := last.Evidence["correct"]; got != true {
		t.Fatalf("Evidence[correct] = %v, want true", got)
	}
	if got := last.Evidence["independent"]; got != false {
		t.Fatalf("Evidence[independent] = %v, want false (answer was revealed before this attempt)", got)
	}
}

// TestCorrectionConfidenceReturns204 pins "click 4 → 204".
func TestCorrectionConfidenceReturns204(t *testing.T) {
	h, _, correctionID, events := socraticRoundTrip(t, "とても面白いでした")

	// Accept first via retry, mirroring the browser flow's order (the
	// confidence widget only appears once accepted — see the template's
	// {{if eq .Status "accepted"}} guard — but the handler/service don't
	// themselves require it, and confidence.recorded firing here still
	// pins the endpoint's core contract).
	if rec := postForm(t, h, "/corrections/"+correctionID+"/retry", url.Values{"attempt": {"面白かったです"}}); rec.Code != http.StatusOK {
		t.Fatalf("POST retry status = %d, body=%s", rec.Code, rec.Body.String())
	}

	form := url.Values{}
	form.Set("confidence", "4")
	rec := postForm(t, h, "/corrections/"+correctionID+"/confidence", form)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST confidence status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}

	var found bool
	for _, ev := range events.byIdentity["dev"] {
		if ev.Type == event.TypeConfidenceRecorded {
			found = true
			if got := ev.Evidence["confidence"]; got != 4 {
				t.Fatalf("Evidence[confidence] = %v, want 4", got)
			}
		}
	}
	if !found {
		t.Fatalf("expected a confidence.recorded event, found none: %+v", events.byIdentity["dev"])
	}
}

// TestCorrectionConfidenceOutOfRangeReturnsBadRequest.
func TestCorrectionConfidenceOutOfRangeReturnsBadRequest(t *testing.T) {
	h, _, correctionID, _ := socraticRoundTrip(t, "とても面白いでした")

	form := url.Values{}
	form.Set("confidence", "0")
	rec := postForm(t, h, "/corrections/"+correctionID+"/confidence", form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestCorrectionRetryUnknownIDReturnsNotFound mirrors
// TestFeedbackRequestCrossIdentitySessionNotFound's shape for the new
// retry route.
func TestCorrectionRetryUnknownIDReturnsNotFound(t *testing.T) {
	h, _, _, _ := socraticFeedbackTestServer(t, "とても面白いでした")

	form := url.Values{}
	form.Set("attempt", "面白かったです")
	rec := postForm(t, h, "/corrections/"+uuidLikeMissingID+"/retry", form)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// uuidLikeMissingID is a syntactically-plausible but never-inserted
// correction id, used by TestCorrectionRetryUnknownIDReturnsNotFound.
const uuidLikeMissingID = "00000000-0000-0000-0000-000000000000"

// TestNonSocraticCorrectionCardHasNoRetryAffordance: a plain
// (non-socratic) correction — the existing Phase 1 shape — must render
// exactly as before Task 8, with no retry input, no hint, no reveal
// button. Guards against the socratic gate accidentally firing for a
// correction whose Hint is the zero Explanation{}.
func TestNonSocraticCorrectionCardHasNoRetryAffordance(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	form.Set("text", content)
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST feedback status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `name="attempt"`) {
		t.Fatalf("non-socratic card unexpectedly has a retry input: %s", body)
	}
	if strings.Contains(body, "答えを見る") {
		t.Fatalf("non-socratic card unexpectedly has a reveal button: %s", body)
	}
	if !strings.Contains(body, "納得した") {
		t.Fatalf("non-socratic card missing the plain accept button: %s", body)
	}
}

// TestCorrectionStatusInvalidValueReturnsBadRequest: the service
// rejects any status other than accepted/rejected.
func TestCorrectionStatusInvalidValueReturnsBadRequest(t *testing.T) {
	content := "とても面白いでした"
	h, sess, docID := feedbackTestServer(t, content)
	runeLen := len([]rune(content))

	form := url.Values{}
	form.Set("document_id", docID)
	form.Set("start", "0")
	form.Set("end", strconv.Itoa(runeLen))
	rec := postForm(t, h, "/sessions/"+string(sess.ID)+"/feedback", form)
	correctionID := extractCorrectionID(t, rec.Body.String())

	statusForm := url.Values{}
	statusForm.Set("status", "bogus")
	statusRec := postForm(t, h, "/corrections/"+correctionID+"/status", statusForm)

	if statusRec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", statusRec.Code, statusRec.Body.String())
	}
}
