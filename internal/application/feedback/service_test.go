package feedback_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"    //nolint:depguard // fakeai/inprocbus are port-shaped test doubles; PRD §75 forbids agents/application importing real adapters, not fakes
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // see fakeai above
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
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
	insertErr   error
}

func newFakeFeedbackRepo() *fakeFeedbackRepo {
	return &fakeFeedbackRepo{
		feedback:    map[string]storage.FeedbackRecord{},
		corrections: map[string]storage.CorrectionRecord{},
	}
}

func (f *fakeFeedbackRepo) InsertFeedback(_ context.Context, rec storage.FeedbackRecord, corrections []storage.CorrectionRecord) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.feedback[rec.ID] = rec
	for _, c := range corrections {
		f.corrections[c.ID] = c
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
	events   *fakeEventStore
}

func newTestHarness() *testHarness {
	sessions := newFakeSessionRepo()
	docs := newFakeDocRepo()
	repo := newFakeFeedbackRepo()
	events := &fakeEventStore{}
	rec := learning.NewRecorder(events, inprocbus.New())
	t := teacher.New(fakeai.New())
	svc := appfeedback.NewService(sessions, docs, repo, t, rec)
	return &testHarness{svc: svc, sessions: sessions, docs: docs, repo: repo, events: events}
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

	// Events recorded in order: feedback.requested, then
	// correction.presented.
	if len(h.events.events) != 2 {
		t.Fatalf("recorded %d events, want 2: %+v", len(h.events.events), h.events.events)
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
