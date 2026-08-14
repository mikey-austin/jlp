package sessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeSessionRepo is an in-memory storage.SessionRepository, keyed by
// identity+"/"+id to mirror the identity-scoped port it fakes.
type fakeSessionRepo struct {
	byKey map[string]session.Session
}

func newFakeSessionRepo() *fakeSessionRepo {
	return &fakeSessionRepo{byKey: map[string]session.Session{}}
}

func fakeKey(identity learner.IdentityID, id session.ID) string {
	return string(identity) + "/" + string(id)
}

func (f *fakeSessionRepo) Create(_ context.Context, s session.Session) error {
	f.byKey[fakeKey(s.IdentityID, s.ID)] = s
	return nil
}

func (f *fakeSessionRepo) Get(_ context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	s, ok := f.byKey[fakeKey(identity, id)]
	if !ok {
		return session.Session{}, storage.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessionRepo) List(_ context.Context, identity learner.IdentityID) ([]session.Session, error) {
	var out []session.Session
	for _, s := range f.byKey {
		if s.IdentityID == identity {
			out = append(out, s)
		}
	}
	return out, nil
}

func TestCreateAppliesDefaultsAndGeneratesID(t *testing.T) {
	svc := sessions.NewService(newFakeSessionRepo())

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
}

func TestCreateExplicitProfileValuesAreNotOverridden(t *testing.T) {
	svc := sessions.NewService(newFakeSessionRepo())

	got, err := svc.Create(context.Background(), "learner-a", "Title", "Diary", session.Profile{
		TeacherMode:         "strict-corrector",
		ExplanationLanguage: "ja",
		Strictness:          "strict",
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
}

func TestCreateEmptyTitleErrors(t *testing.T) {
	svc := sessions.NewService(newFakeSessionRepo())

	_, err := svc.Create(context.Background(), "learner-a", "", "Diary", session.Profile{})
	if err == nil {
		t.Fatal("expected an error for an empty title")
	}
}

func TestListReturnsOnlyCallerIdentitySessions(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := sessions.NewService(repo)

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
	svc := sessions.NewService(repo)

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
