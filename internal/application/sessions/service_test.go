package sessions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	//nolint:depguard // inprocbus is the port-shaped EventBus double the
	// delete/restore tests inject through learning.NewRecorder, exactly
	// as production wiring does — the same exemption
	// application/vocabulary/service_test.go carries for the same reason.
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeSessionRepo is an in-memory storage.SessionRepository, keyed by
// identity+"/"+id to mirror the identity-scoped port it fakes.
//
// deleted mirrors the real adapter's deleted_at column, and Get/List
// honour it, because a fake that ignored soft delete would let the
// service's Delete tests pass while the thing they are actually
// asserting — "it stops coming back from reads" — went untested.
type fakeSessionRepo struct {
	byKey   map[string]session.Session
	deleted map[string]time.Time
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byKey: map[string]session.Session{}, deleted: map[string]time.Time{}}
}

func fakeKey(identity learner.IdentityID, id session.ID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeSessionRepo) Create(_ context.Context, s session.Session) error {
	f.byKey[fakeKey(s.IdentityID, s.ID)] = s
	return nil
}

func (f *fakeSessionRepo) Get(_ context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	key := fakeKey(identity, id)
	s, ok := f.byKey[key]
	if !ok {
		return session.Session{}, storage.ErrNotFound
	}
	if _, gone := f.deleted[key]; gone {
		return session.Session{}, storage.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessionRepo) List(_ context.Context, identity learner.IdentityID) ([]session.Session, error) {
	var out []session.Session
	for key, s := range f.byKey {
		if s.IdentityID != identity {
			continue
		}
		if _, gone := f.deleted[key]; gone {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// SoftDelete mirrors the real adapter's contract exactly: identity-
// scoped (a key that includes the identity), idempotent (a repeat
// delete keeps the first timestamp), and ErrNotFound for both "unknown
// id" and "someone else's id" — the property the cross-identity tests
// below rely on.
func (f *fakeSessionRepo) SoftDelete(_ context.Context, identity learner.IdentityID, id session.ID, at time.Time) error {
	key := fakeKey(identity, id)
	if _, ok := f.byKey[key]; !ok {
		return storage.ErrNotFound
	}
	if _, already := f.deleted[key]; !already {
		f.deleted[key] = at
	}
	return nil
}

func (f *fakeSessionRepo) Restore(_ context.Context, identity learner.IdentityID, id session.ID) error {
	key := fakeKey(identity, id)
	if _, ok := f.byKey[key]; !ok {
		return storage.ErrNotFound
	}
	delete(f.deleted, key)
	return nil
}

// isDeleted lets a test assert on the raw storage state rather than on
// what a read returns — the difference between "the delete was refused"
// and "the row was quietly removed anyway".
func (f *fakeSessionRepo) isDeleted(identity learner.IdentityID, id session.ID) bool {
	_, gone := f.deleted[fakeKey(identity, id)]
	return gone
}

// exists reports whether the row is still stored at all, deleted or
// not. A cross-identity test that only asserted ErrNotFound would pass
// even if the row had been destroyed; this is what makes that
// assertion mean something.
func (f *fakeSessionRepo) exists(identity learner.IdentityID, id session.ID) bool {
	_, ok := f.byKey[fakeKey(identity, id)]
	return ok
}

// recordingEventStore is a storage.LearningEventRepository that keeps
// what it was handed, so the delete tests can assert the audit event
// was actually appended rather than trusting that it was.
type recordingEventStore struct{ appended []event.LearningEvent }

func (s *recordingEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	s.appended = append(s.appended, ev)
	return nil
}

func (s *recordingEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return nil, nil
}

func (s *recordingEventStore) ListAll(context.Context, learner.IdentityID) ([]event.LearningEvent, error) {
	return nil, nil
}

// newTestService wires a Service over repo with a real Recorder, so the
// event-recording half of Delete/Restore is exercised rather than
// stubbed out.
func newTestService(repo storage.SessionRepository) (*sessions.Service, *recordingEventStore) {
	store := &recordingEventStore{}
	return sessions.NewService(repo, learning.NewRecorder(store, inprocbus.New())), store
}

func newService(repo storage.SessionRepository) *sessions.Service {
	svc, _ := newTestService(repo)
	return svc
}

func TestCreateAppliesDefaultsAndGeneratesID(t *testing.T) {
	svc := newService(newFakeSessionRepo())

	got, err := svc.Create(context.Background(), "learner-a", "旅行について書く", "Blog post", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected a generated ID")
	}
	if got.Profile.TeacherMode != "teacher" {
		t.Fatalf("TeacherMode = %q, want %q", got.Profile.TeacherMode, "teacher")
	}
	if got.Profile.ExplanationLanguage != "both" {
		t.Fatalf("ExplanationLanguage = %q, want %q", got.Profile.ExplanationLanguage, "both")
	}
	if got.Profile.Strictness != "balanced" {
		t.Fatalf("Strictness = %q, want %q", got.Profile.Strictness, "balanced")
	}
	// PRD §17.4: a new session defaults to "end" feedback timing — the
	// conversation tutor withholds corrections entirely until
	// Summarise, never interrupting the dialogue — not "immediate".
	if got.Profile.FeedbackTiming != "end" {
		t.Fatalf("FeedbackTiming = %q, want %q", got.Profile.FeedbackTiming, "end")
	}
}

func TestCreateExplicitProfileValuesAreNotOverridden(t *testing.T) {
	svc := newService(newFakeSessionRepo())

	got, err := svc.Create(context.Background(), "learner-a", "Title", "Diary", session.Profile{
		TeacherMode:         "strict-corrector",
		ExplanationLanguage: "ja",
		Strictness:          "strict",
		FeedbackTiming:      "immediate",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if got.Profile.TeacherMode != "strict-corrector" {
		t.Fatalf("TeacherMode = %q, want %q", got.Profile.TeacherMode, "strict-corrector")
	}
	if got.Profile.ExplanationLanguage != "ja" {
		t.Fatalf("ExplanationLanguage = %q, want %q", got.Profile.ExplanationLanguage, "ja")
	}
	if got.Profile.Strictness != "strict" {
		t.Fatalf("Strictness = %q, want %q", got.Profile.Strictness, "strict")
	}
	if got.Profile.FeedbackTiming != "immediate" {
		t.Fatalf("FeedbackTiming = %q, want %q", got.Profile.FeedbackTiming, "immediate")
	}
}

func TestCreateEmptyTitleErrors(t *testing.T) {
	svc := newService(newFakeSessionRepo())

	_, err := svc.Create(context.Background(), "learner-a", "", "Diary", session.Profile{})
	if !errors.Is(err, sessions.ErrInvalidTitle) {
		t.Fatalf("Create error = %v, want sessions.ErrInvalidTitle", err)
	}
}

func TestListReturnsOnlyCallerIdentitySessions(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)

	if _, err := svc.Create(context.Background(), "learner-a", "A's session", "Diary", session.Profile{}); err != nil {
		t.Fatalf("Create (a) returned error: %v", err)
	}
	if _, err := svc.Create(context.Background(), "learner-b", "B's session", "Diary", session.Profile{}); err != nil {
		t.Fatalf("Create (b) returned error: %v", err)
	}

	got, err := svc.List(context.Background(), "learner-a")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d sessions, want 1", len(got))
	}
	if got[0].Title != "A's session" {
		t.Fatalf("List[0].Title = %q, want %q", got[0].Title, "A's session")
	}
}

func TestGetForAnotherIdentityReturnsErrNotFound(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)

	created, err := svc.Create(context.Background(), "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	_, err = svc.Get(context.Background(), "learner-b", created.ID)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get error = %v, want storage.ErrNotFound", err)
	}

	// Sanity check: the owning identity can still fetch it.
	got, err := svc.Get(context.Background(), "learner-a", created.ID)
	if err != nil {
		t.Fatalf("Get (owner) returned error: %v", err)
	}
	if got.Title != "A's session" {
		t.Fatalf("Get (owner).Title = %q, want %q", got.Title, "A's session")
	}
}
