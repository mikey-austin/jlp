package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type fakeAuth struct {
	id  learner.Identity
	err error
}

func (f fakeAuth) Authenticate(*http.Request) (learner.Identity, error) { return f.id, f.err }

type fakeIdentityRepo struct{ upserts int }

func (f *fakeIdentityRepo) Upsert(context.Context, learner.Identity) error { f.upserts++; return nil }
func (f *fakeIdentityRepo) Get(_ context.Context, id learner.IdentityID) (learner.Identity, error) {
	return learner.Identity{ID: id}, nil
}

func TestRequireIdentityInjectsAndUpsertsOnce(t *testing.T) {
	repo := &fakeIdentityRepo{}
	mw := RequireIdentity(fakeAuth{id: learner.Identity{ID: "dev", DisplayName: "Dev"}}, repo)
	var seen learner.Identity
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = IdentityFrom(r.Context())
	}))
	for range 3 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}
	if seen.ID != "dev" || repo.upserts != 1 {
		t.Fatalf("seen=%+v upserts=%d", seen, repo.upserts)
	}
}

func TestRequireIdentityRejects(t *testing.T) {
	mw := RequireIdentity(fakeAuth{err: errors.New("nope")}, &fakeIdentityRepo{})
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}
