package anki_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	agentanki "github.com/mikeyaustin/jlp/internal/agent/anki"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const testIdentity = learner.IdentityID("learner-a")

// fakeAnkiCardRepo is an in-memory storage.AnkiCardRepository:
// identity-scoped UpdateStatus, same miss semantics as the real
// postgres adapter — mirrors internal/application/practice/
// service_test.go's fakeExerciseRepo shape. mu guards every method
// (not just TakeApprovedForExport) so this double is safe to drive from
// concurrent goroutines the same way the real postgres repository's
// per-transaction locking is.
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

func (f *fakeAnkiCardRepo) List(_ context.Context, identity learner.IdentityID, status string) ([]storage.AnkiCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storage.AnkiCard
	for _, c := range f.byID {
		if c.IdentityID != identity {
			continue
		}
		if status != "" && c.Status != status {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeAnkiCardRepo) UpdateStatus(_ context.Context, identity learner.IdentityID, id, status string) (storage.AnkiCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.byID[id]
	if !ok || c.IdentityID != identity {
		return storage.AnkiCard{}, storage.ErrNotFound
	}
	c.Status = status
	f.byID[id] = c
	return c, nil
}

// TakeApprovedForExport mirrors the real postgres repository's
// SELECT-FOR-UPDATE-then-UPDATE atomicity: holding mu for the whole
// read+mark means two goroutines calling this concurrently for the same
// identity can never both see the same approved row — mirrors
// storage.AnkiCardRepository's documented concurrency contract.
func (f *fakeAnkiCardRepo) TakeApprovedForExport(_ context.Context, identity learner.IdentityID, _ time.Time) ([]storage.AnkiCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storage.AnkiCard
	for id, c := range f.byID {
		if c.IdentityID == identity && c.Status == "approved" {
			c.Status = "exported"
			f.byID[id] = c
			out = append(out, c)
		}
	}
	return out, nil
}

// fakeFeedbackRepo is a minimal storage.FeedbackRepository double: Anki
// service tests only ever call GetCorrection, so every other method
// panics if actually invoked — mirrors this codebase's "needed only for
// the constructor/interface" convention (see e.g.
// application/practice/service_test.go's fakeObsRepo).
type fakeFeedbackRepo struct {
	corrections map[string]storage.CorrectionRecord
	// identities maps correction ID -> owning identity, since
	// storage.CorrectionRecord itself carries no IdentityID field (the
	// real repo learns it via a join to feedback_requests — see
	// storage.FeedbackRepository's doc comment).
	identities map[string]learner.IdentityID
}

func newFakeFeedbackRepo() *fakeFeedbackRepo {
	return &fakeFeedbackRepo{corrections: map[string]storage.CorrectionRecord{}, identities: map[string]learner.IdentityID{}}
}

func (f *fakeFeedbackRepo) seed(identity learner.IdentityID, rec storage.CorrectionRecord) {
	f.corrections[rec.ID] = rec
	f.identities[rec.ID] = identity
}

func (f *fakeFeedbackRepo) GetCorrection(_ context.Context, identity learner.IdentityID, correctionID string) (storage.CorrectionRecord, error) {
	rec, ok := f.corrections[correctionID]
	if !ok || f.identities[correctionID] != identity {
		return storage.CorrectionRecord{}, storage.ErrNotFound
	}
	return rec, nil
}

func (f *fakeFeedbackRepo) InsertFeedback(context.Context, storage.FeedbackRecord, []storage.CorrectionRecord, map[string][]storage.ConceptTag) error {
	panic("not used by anki service tests")
}

func (f *fakeFeedbackRepo) UpdateCorrectionStatus(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	panic("not used by anki service tests")
}

func (f *fakeFeedbackRepo) GetCorrectionConcepts(context.Context, string) ([]string, error) {
	panic("not used by anki service tests")
}

func (f *fakeFeedbackRepo) RetryCorrection(context.Context, learner.IdentityID, string, string) (storage.CorrectionRecord, error) {
	panic("not used by anki service tests")
}

func (f *fakeFeedbackRepo) RevealCorrection(context.Context, learner.IdentityID, string) (storage.CorrectionRecord, error) {
	panic("not used by anki service tests")
}

func (f *fakeFeedbackRepo) RecordConfidence(context.Context, learner.IdentityID, string, int) (storage.CorrectionRecord, error) {
	panic("not used by anki service tests")
}

// fakeCapturingEventStore actually records every Append call — mirrors
// application/practice/service_test.go's double of the same name.
// appendErr, when set, fails every Append (without recording it) —
// used to pin the log-and-continue contract on anki.card.created/
// anki.card.exported (see service.go's recordExported doc comment).
type fakeCapturingEventStore struct {
	appended  []event.LearningEvent
	appendErr error
}

func (f *fakeCapturingEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appended = append(f.appended, ev)
	return nil
}
func (f *fakeCapturingEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	panic("not used by anki service tests")
}
func (f *fakeCapturingEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	panic("not used by anki service tests")
}

// fakeConnector is an appanki.AnkiConnector test double: it records
// every call and answers with a configurable (added, err) pair.
type fakeConnector struct {
	calls [][]storage.AnkiCard
	added int
	err   error
}

func (f *fakeConnector) AddNotes(_ context.Context, cards []storage.AnkiCard) (int, error) {
	f.calls = append(f.calls, cards)
	return f.added, f.err
}

type testHarness struct {
	svc      *appanki.Service
	cards    *fakeAnkiCardRepo
	feedback *fakeFeedbackRepo
	events   *fakeCapturingEventStore
}

func newTestHarness() *testHarness {
	cards := newFakeAnkiCardRepo()
	feedback := newFakeFeedbackRepo()
	events := &fakeCapturingEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	agent := agentanki.New(fakeai.New())
	svc := appanki.NewService(cards, feedback, agent, rec)
	return &testHarness{svc: svc, cards: cards, feedback: feedback, events: events}
}

const testSessionID = session.ID("sess-1")

func testCorrection(id string) storage.CorrectionRecord {
	return storage.CorrectionRecord{
		ID:            id,
		SessionID:     testSessionID,
		Original:      "面白いでした",
		Replacement:   "面白かったです",
		Type:          "conjugation",
		Severity:      "incorrect",
		ExplanationJA: "い形容詞の過去形は「〜かった」を使います。",
		ExplanationEN: "い-adjectives form the past tense with 〜かった.",
		Status:        "accepted",
	}
}

// --- GenerateFromCorrection ---

// TestGenerateFromCorrectionCreatesDraftAndEvent pins the brief's Step 1
// scenario: a draft card is persisted with the canned fakeai
// front/back/notes, and anki.card.created fires with source_type/
// source_id Evidence.
func TestGenerateFromCorrectionCreatesDraftAndEvent(t *testing.T) {
	h := newTestHarness()
	h.feedback.seed(testIdentity, testCorrection("corr-1"))

	card, err := h.svc.GenerateFromCorrection(context.Background(), testIdentity, "corr-1")
	if err != nil {
		t.Fatalf("GenerateFromCorrection returned error: %v", err)
	}
	if card.Status != "draft" {
		t.Fatalf("Status = %q, want draft", card.Status)
	}
	if card.SourceType != "correction" || card.SourceID != "corr-1" {
		t.Fatalf("SourceType/SourceID = %q/%q, want correction/corr-1", card.SourceType, card.SourceID)
	}
	if card.Front != "「とても面白いでした」— 何が不自然？" {
		t.Fatalf("Front = %q, want the canned front", card.Front)
	}
	if !strings.Contains(card.Back, "「とても面白かったです」") {
		t.Fatalf("Back = %q, want it to contain the canned corrected form", card.Back)
	}
	if card.ID == "" {
		t.Fatal("GenerateFromCorrection did not assign an ID")
	}
	if card.CreatedAt.IsZero() {
		t.Fatal("GenerateFromCorrection did not assign CreatedAt")
	}

	persisted, ok := h.cards.byID[card.ID]
	if !ok {
		t.Fatal("GenerateFromCorrection did not persist the card")
	}
	if persisted.Front != card.Front {
		t.Fatalf("persisted.Front = %q, want %q", persisted.Front, card.Front)
	}

	if len(h.events.appended) != 1 {
		t.Fatalf("appended events = %d, want 1: %+v", len(h.events.appended), h.events.appended)
	}
	ev := h.events.appended[0]
	if ev.Type != event.TypeAnkiCardCreated {
		t.Fatalf("event type = %q, want %q", ev.Type, event.TypeAnkiCardCreated)
	}
	if ev.Subject != card.ID {
		t.Fatalf("event subject = %q, want %q", ev.Subject, card.ID)
	}
	if ev.Evidence["source_type"] != "correction" || ev.Evidence["source_id"] != "corr-1" {
		t.Fatalf("event evidence = %+v, want source_type=correction source_id=corr-1", ev.Evidence)
	}
	if ev.SessionID == nil || *ev.SessionID != testSessionID {
		t.Fatalf("event SessionID = %v, want %q (the correction's owning session, so it shows in that session's activity feed)", ev.SessionID, testSessionID)
	}
}

// TestGenerateFromCorrectionCrossIdentityMisses pins the brief's
// cross-identity ErrNotFound contract.
func TestGenerateFromCorrectionCrossIdentityMisses(t *testing.T) {
	h := newTestHarness()
	h.feedback.seed(testIdentity, testCorrection("corr-1"))

	_, err := h.svc.GenerateFromCorrection(context.Background(), learner.IdentityID("someone-else"), "corr-1")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestGenerateFromCorrectionUnknownIDMisses pins the plain not-found case.
func TestGenerateFromCorrectionUnknownIDMisses(t *testing.T) {
	h := newTestHarness()
	_, err := h.svc.GenerateFromCorrection(context.Background(), testIdentity, "does-not-exist")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestGenerateFromCorrectionSurvivesEventRecordFailure pins the
// code-review fix: repo.Insert has already committed the draft card by
// the time the anki.card.created Record call runs, so a Recorder
// failure there must not turn a successfully created draft into a
// reported 500 — the card would otherwise exist in storage while the
// caller was told generation failed.
func TestGenerateFromCorrectionSurvivesEventRecordFailure(t *testing.T) {
	h := newTestHarness()
	h.feedback.seed(testIdentity, testCorrection("corr-1"))
	h.events.appendErr = errors.New("event store unavailable")

	card, err := h.svc.GenerateFromCorrection(context.Background(), testIdentity, "corr-1")
	if err != nil {
		t.Fatalf("GenerateFromCorrection returned error: %v, want nil (event-record failures must be logged, not surfaced)", err)
	}
	if card.Status != "draft" {
		t.Fatalf("Status = %q, want draft", card.Status)
	}
	if _, ok := h.cards.byID[card.ID]; !ok {
		t.Fatal("GenerateFromCorrection did not persist the card despite the event-record failure")
	}
}

// --- SetStatus ---

func draftCard(t *testing.T, h *testHarness) storage.AnkiCard {
	t.Helper()
	h.feedback.seed(testIdentity, testCorrection("corr-1"))
	card, err := h.svc.GenerateFromCorrection(context.Background(), testIdentity, "corr-1")
	if err != nil {
		t.Fatalf("GenerateFromCorrection returned error: %v", err)
	}
	return card
}

// TestSetStatusApprovedThenExportTransition pins the brief's
// draft→approved→exported status transition (the "exported" half is
// covered by TestExportTSV* below; this pins draft→approved).
func TestSetStatusApprovedThenExportTransition(t *testing.T) {
	h := newTestHarness()
	card := draftCard(t, h)

	got, err := h.svc.SetStatus(context.Background(), testIdentity, card.ID, "approved")
	if err != nil {
		t.Fatalf("SetStatus returned error: %v", err)
	}
	if got.Status != "approved" {
		t.Fatalf("Status = %q, want approved", got.Status)
	}
}

// TestSetStatusRejected pins the reject path.
func TestSetStatusRejected(t *testing.T) {
	h := newTestHarness()
	card := draftCard(t, h)

	got, err := h.svc.SetStatus(context.Background(), testIdentity, card.ID, "rejected")
	if err != nil {
		t.Fatalf("SetStatus returned error: %v", err)
	}
	if got.Status != "rejected" {
		t.Fatalf("Status = %q, want rejected", got.Status)
	}
}

// TestSetStatusInvalidValueErrors pins ErrInvalidStatus for anything
// other than approved/rejected.
func TestSetStatusInvalidValueErrors(t *testing.T) {
	h := newTestHarness()
	card := draftCard(t, h)

	_, err := h.svc.SetStatus(context.Background(), testIdentity, card.ID, "bogus")
	if !errors.Is(err, appanki.ErrInvalidStatus) {
		t.Fatalf("err = %v, want ErrInvalidStatus", err)
	}
}

// TestSetStatusCrossIdentityMisses pins the cross-identity contract.
func TestSetStatusCrossIdentityMisses(t *testing.T) {
	h := newTestHarness()
	card := draftCard(t, h)

	_, err := h.svc.SetStatus(context.Background(), learner.IdentityID("someone-else"), card.ID, "approved")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// --- ExportTSV ---

func approvedCard(t *testing.T, h *testHarness, correctionID string) storage.AnkiCard {
	t.Helper()
	h.feedback.seed(testIdentity, testCorrection(correctionID))
	card, err := h.svc.GenerateFromCorrection(context.Background(), testIdentity, correctionID)
	if err != nil {
		t.Fatalf("GenerateFromCorrection returned error: %v", err)
	}
	got, err := h.svc.SetStatus(context.Background(), testIdentity, card.ID, "approved")
	if err != nil {
		t.Fatalf("SetStatus returned error: %v", err)
	}
	return got
}

// TestExportTSVFormatAndMarksExported pins the brief's TSV format
// (`front\tback\n`, no header) and the status transition to "exported".
// The canned card's own Back — "「とても面白かったです」\n\n理由: ..." —
// already contains an embedded blank line (exactly the shape the
// anki.generate.v1 prompt asks the model to produce for every real
// card: corrected form, then a blank line, then 理由 on its own line),
// so this is also the canonical sanitization case, not a contrived one:
// the exported line must still be exactly ONE line with exactly ONE
// tab, with the embedded newlines turned into "<br>" rather than
// splitting the TSV record.
func TestExportTSVFormatAndMarksExported(t *testing.T) {
	h := newTestHarness()
	card := approvedCard(t, h, "corr-1")

	tsv, ids, err := h.svc.ExportTSV(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("ExportTSV returned error: %v", err)
	}
	if !strings.Contains(card.Back, "\n") {
		t.Fatal("precondition failed: the canned card's Back has no embedded newline to sanitize")
	}
	wantBack := strings.ReplaceAll(card.Back, "\n", "<br>")
	want := card.Front + "\t" + wantBack + "\n"
	if string(tsv) != want {
		t.Fatalf("tsv = %q, want %q", tsv, want)
	}
	if len(ids) != 1 || ids[0] != card.ID {
		t.Fatalf("ids = %v, want [%s]", ids, card.ID)
	}
	// The card's Back has an embedded blank line (two "\n"s) — proving
	// they were sanitized away, not just that SOME substitution
	// happened, requires counting: exactly one newline in the whole
	// export (the row terminator), matching len(cards)=1.
	if got := strings.Count(string(tsv), "\n"); got != 1 {
		t.Fatalf("newline count = %d, want 1 (one row terminator per card; the embedded \\n\\n in Back must not survive)", got)
	}
	if got := strings.Count(string(tsv), "\t"); got != 1 {
		t.Fatalf("tab count = %d, want 1 (exactly one front/back separator)", got)
	}

	got := h.cards.byID[card.ID]
	if got.Status != "exported" {
		t.Fatalf("Status = %q, want exported", got.Status)
	}
}

// TestExportTSVSanitizesEmbeddedTabsAndNewlines pins the fix directly
// against a fabricated card whose Back contains a tab AND every kind of
// line ending (\n, \r\n, \r) — the exact scenario the code review's
// Finding 1 called out: a raw embedded tab/newline corrupts the TSV
// record it appears in (a tab shifts every later field into the wrong
// column for that row; a newline ends the row early, splitting one
// card into two garbled lines in Anki's importer).
func TestExportTSVSanitizesEmbeddedTabsAndNewlines(t *testing.T) {
	h := newTestHarness()
	card := storage.AnkiCard{
		ID:         "card-with-tricky-fields",
		IdentityID: testIdentity,
		SourceType: "correction",
		SourceID:   "corr-1",
		Front:      "front\twith\ttabs",
		Back:       "A\n\n理由: B\r\nC\rD",
		Status:     "approved",
		CreatedAt:  time.Now().UTC(),
	}
	if err := h.cards.Insert(context.Background(), card); err != nil {
		t.Fatalf("Insert returned error: %v", err)
	}

	tsv, ids, err := h.svc.ExportTSV(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("ExportTSV returned error: %v", err)
	}
	want := "front with tabs\tA<br><br>理由: B<br>C<br>D\n"
	if string(tsv) != want {
		t.Fatalf("tsv = %q, want %q", tsv, want)
	}
	if len(ids) != 1 || ids[0] != card.ID {
		t.Fatalf("ids = %v, want [%s]", ids, card.ID)
	}
	// Exactly one line (one card, one row terminator) — not the several
	// an unsanitized embedded "\n\n" plus "\r\n" plus "\r" would produce.
	if got := strings.Count(string(tsv), "\n"); got != 1 {
		t.Fatalf("newline count = %d, want 1 (== number of cards)", got)
	}
	// Exactly one tab (the front/back column separator) — the embedded
	// tabs in Front must have been replaced with spaces, not left to
	// masquerade as extra column separators.
	if got := strings.Count(string(tsv), "\t"); got != 1 {
		t.Fatalf("tab count = %d, want 1 (the embedded tabs in Front must be sanitized, not counted as separators)", got)
	}
	if !strings.Contains(string(tsv), "<br>") {
		t.Fatal("tsv missing the <br> substitution for embedded newlines")
	}
}

// TestExportTSVExcludesRejectedAndDraft pins "rejected excluded from
// export" (and draft too — only "approved" cards are ever exported).
func TestExportTSVExcludesRejectedAndDraft(t *testing.T) {
	h := newTestHarness()
	approved := approvedCard(t, h, "corr-approved")

	h.feedback.seed(testIdentity, testCorrection("corr-draft"))
	if _, err := h.svc.GenerateFromCorrection(context.Background(), testIdentity, "corr-draft"); err != nil {
		t.Fatalf("GenerateFromCorrection returned error: %v", err)
	}

	h.feedback.seed(testIdentity, testCorrection("corr-rejected"))
	rejectedCard, err := h.svc.GenerateFromCorrection(context.Background(), testIdentity, "corr-rejected")
	if err != nil {
		t.Fatalf("GenerateFromCorrection returned error: %v", err)
	}
	if _, err := h.svc.SetStatus(context.Background(), testIdentity, rejectedCard.ID, "rejected"); err != nil {
		t.Fatalf("SetStatus returned error: %v", err)
	}

	tsv, ids, err := h.svc.ExportTSV(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("ExportTSV returned error: %v", err)
	}
	if len(ids) != 1 || ids[0] != approved.ID {
		t.Fatalf("ids = %v, want exactly [%s] (draft/rejected excluded)", ids, approved.ID)
	}
	// Exactly one row: one tab (front/back separator) AND one newline
	// (row terminator). Asserting on "\n" count is safe now that
	// ExportTSV sanitizes embedded newlines (see sanitizeTSVField) —
	// the canned back text's own embedded "\n\n" (the front/理由 line
	// break) is turned into "<br>" before this count runs, so a
	// newline-count of 1 genuinely means "one card," not an artifact of
	// picking a metric that happened to dodge the multi-line back text.
	if got := strings.Count(string(tsv), "\t"); got != 1 {
		t.Fatalf("tab count = %d, want exactly 1", got)
	}
	if got := strings.Count(string(tsv), "\n"); got != 1 {
		t.Fatalf("newline count = %d, want exactly 1 (== number of exported cards)", got)
	}
}

// TestExportTSVSecondImmediateCallReturnsEmpty pins the controller
// ruling's safety property: a second ExportTSV call right after the
// first sees no approved cards left (the first call already marked
// them exported) — no double export.
func TestExportTSVSecondImmediateCallReturnsEmpty(t *testing.T) {
	h := newTestHarness()
	approvedCard(t, h, "corr-1")

	first, ids1, err := h.svc.ExportTSV(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("first ExportTSV returned error: %v", err)
	}
	if len(first) == 0 || len(ids1) == 0 {
		t.Fatalf("first ExportTSV = %q/%v, want non-empty", first, ids1)
	}

	second, ids2, err := h.svc.ExportTSV(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("second ExportTSV returned error: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second ExportTSV tsv = %q, want empty (no double export)", second)
	}
	if len(ids2) != 0 {
		t.Fatalf("second ExportTSV ids = %v, want empty", ids2)
	}
}

// TestExportTSVNoApprovedCardsReturnsEmptyNotError pins the empty-set
// path: no error, just an empty result.
func TestExportTSVNoApprovedCardsReturnsEmptyNotError(t *testing.T) {
	h := newTestHarness()
	tsv, ids, err := h.svc.ExportTSV(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("ExportTSV returned error: %v", err)
	}
	if len(tsv) != 0 || len(ids) != 0 {
		t.Fatalf("tsv/ids = %q/%v, want both empty", tsv, ids)
	}
}

// TestExportTSVRecordsExportedEvent pins the anki.card.exported event.
func TestExportTSVRecordsExportedEvent(t *testing.T) {
	h := newTestHarness()
	card := approvedCard(t, h, "corr-1")
	h.events.appended = nil // discard the anki.card.created event

	if _, _, err := h.svc.ExportTSV(context.Background(), testIdentity); err != nil {
		t.Fatalf("ExportTSV returned error: %v", err)
	}
	if len(h.events.appended) != 1 {
		t.Fatalf("appended events = %d, want 1: %+v", len(h.events.appended), h.events.appended)
	}
	ev := h.events.appended[0]
	if ev.Type != event.TypeAnkiCardExported || ev.Subject != card.ID {
		t.Fatalf("event = %+v, want anki.card.exported for %s", ev, card.ID)
	}
}

// TestExportTSVSurvivesEventRecordFailure pins the code-review fix:
// MarkExported has already committed by the time recordExported runs,
// so a Recorder failure there must NOT discard the already-built TSV
// bytes/ids or fail the call — the learner still needs their file, and
// the card is already durably exported either way.
func TestExportTSVSurvivesEventRecordFailure(t *testing.T) {
	h := newTestHarness()
	card := approvedCard(t, h, "corr-1")
	h.events.appendErr = errors.New("event store unavailable")

	tsv, ids, err := h.svc.ExportTSV(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("ExportTSV returned error: %v, want nil (event-record failures must be logged, not surfaced)", err)
	}
	if len(tsv) == 0 {
		t.Fatal("ExportTSV discarded its TSV bytes on an event-record failure")
	}
	if len(ids) != 1 || ids[0] != card.ID {
		t.Fatalf("ids = %v, want [%s]", ids, card.ID)
	}
	if h.cards.byID[card.ID].Status != "exported" {
		t.Fatalf("Status = %q, want exported (TakeApprovedForExport already committed before the failed Record call)", h.cards.byID[card.ID].Status)
	}
}

// --- PushToAnkiConnect ---

// TestPushToAnkiConnectNotConfiguredErrors pins the config-gated
// contract: with no SetConnector call, PushToAnkiConnect fails fast.
func TestPushToAnkiConnectNotConfiguredErrors(t *testing.T) {
	h := newTestHarness()
	_, err := h.svc.PushToAnkiConnect(context.Background(), testIdentity)
	if !errors.Is(err, appanki.ErrAnkiConnectNotConfigured) {
		t.Fatalf("err = %v, want ErrAnkiConnectNotConfigured", err)
	}
}

// TestPushToAnkiConnectHappyPathMarksExported pins the configured path:
// every approved card is pushed, all accepted, so the batch is marked
// exported and an event fires.
func TestPushToAnkiConnectHappyPathMarksExported(t *testing.T) {
	h := newTestHarness()
	card := approvedCard(t, h, "corr-1")
	conn := &fakeConnector{added: 1}
	h.svc.SetConnector(conn)

	added, err := h.svc.PushToAnkiConnect(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("PushToAnkiConnect returned error: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if len(conn.calls) != 1 || len(conn.calls[0]) != 1 || conn.calls[0][0].ID != card.ID {
		t.Fatalf("connector calls = %+v, want one call with [%s]", conn.calls, card.ID)
	}
	if h.cards.byID[card.ID].Status != "exported" {
		t.Fatalf("Status = %q, want exported", h.cards.byID[card.ID].Status)
	}
}

// TestPushToAnkiConnectPartialFailureDoesNotMarkExported pins the
// "only leave exported when every card was accepted" safety rule:
// TakeApprovedForExport marks the card "exported" up front (it must, to
// be atomic with the read — see storage.AnkiCardRepository's doc
// comment), but a partial AddNotes result reverts it straight back to
// "approved" (never silently left "exported" for a card Anki didn't
// actually take) and surfaces as an error.
func TestPushToAnkiConnectPartialFailureDoesNotMarkExported(t *testing.T) {
	h := newTestHarness()
	card := approvedCard(t, h, "corr-1")
	conn := &fakeConnector{added: 0} // AnkiConnect accepted none of the 1 card
	h.svc.SetConnector(conn)

	if _, err := h.svc.PushToAnkiConnect(context.Background(), testIdentity); err == nil {
		t.Fatal("expected an error on partial AddNotes success, got nil")
	}
	if h.cards.byID[card.ID].Status != "approved" {
		t.Fatalf("Status = %q, want reverted back to approved (nothing left exported on partial failure)", h.cards.byID[card.ID].Status)
	}
}

// TestPushToAnkiConnectNoApprovedCardsIsNoop pins the empty-set path:
// no error, connector never called.
func TestPushToAnkiConnectNoApprovedCardsIsNoop(t *testing.T) {
	h := newTestHarness()
	conn := &fakeConnector{}
	h.svc.SetConnector(conn)

	added, err := h.svc.PushToAnkiConnect(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("PushToAnkiConnect returned error: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}
	if len(conn.calls) != 0 {
		t.Fatalf("connector calls = %d, want 0 (never called with nothing approved)", len(conn.calls))
	}
}

// TestPushToAnkiConnectConnectorErrorPropagates pins error propagation
// from AddNotes, and that the taken card is reverted back to
// "approved" (not left "exported" for a card that never actually
// reached AnkiConnect at all).
func TestPushToAnkiConnectConnectorErrorPropagates(t *testing.T) {
	h := newTestHarness()
	card := approvedCard(t, h, "corr-1")
	boom := errors.New("boom")
	conn := &fakeConnector{err: boom}
	h.svc.SetConnector(conn)

	if _, err := h.svc.PushToAnkiConnect(context.Background(), testIdentity); err == nil {
		t.Fatal("expected an error when AddNotes fails, got nil")
	}
	if h.cards.byID[card.ID].Status != "approved" {
		t.Fatalf("Status = %q, want reverted back to approved", h.cards.byID[card.ID].Status)
	}
}

// TestPushToAnkiConnectSurvivesEventRecordFailure mirrors
// TestExportTSVSurvivesEventRecordFailure for the AnkiConnect path:
// AddNotes already succeeded and TakeApprovedForExport already
// committed the "exported" mark by the time recordExported runs, so a
// Recorder failure there must not turn a genuinely successful push
// into a reported failure.
func TestPushToAnkiConnectSurvivesEventRecordFailure(t *testing.T) {
	h := newTestHarness()
	card := approvedCard(t, h, "corr-1")
	conn := &fakeConnector{added: 1}
	h.svc.SetConnector(conn)
	h.events.appendErr = errors.New("event store unavailable")

	added, err := h.svc.PushToAnkiConnect(context.Background(), testIdentity)
	if err != nil {
		t.Fatalf("PushToAnkiConnect returned error: %v, want nil (event-record failures must be logged, not surfaced)", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if h.cards.byID[card.ID].Status != "exported" {
		t.Fatalf("Status = %q, want exported", h.cards.byID[card.ID].Status)
	}
}
