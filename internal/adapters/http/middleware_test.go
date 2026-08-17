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

type fakeIdentityRepo struct {
	upserts int
	err     error
}

func (f *fakeIdentityRepo) Upsert(context.Context, learner.Identity) error {
	f.upserts++
	return f.err
}
func (f *fakeIdentityRepo) Get(_ context.Context, id learner.IdentityID) (learner.Identity, error) {
	return learner.Identity{ID: id}, nil
}
func (f *fakeIdentityRepo) ListIdentities(context.Context) ([]learner.Identity, error) {
	return nil, nil
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

func TestRequireIdentityUpsertErrorReturns500AndBlocksHandler(t *testing.T) {
	repo := &fakeIdentityRepo{err: errors.New("db down")}
	called := false
	mw := RequireIdentity(fakeAuth{id: learner.Identity{ID: "dev", DisplayName: "Dev"}}, repo)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", rec.Code)
	}
	if called {
		t.Fatalf("downstream handler must not be called when upsert fails")
	}
	if repo.upserts != 1 {
		t.Fatalf("upserts=%d, want 1", repo.upserts)
	}
}

func TestRequireIdentityRetriesUpsertAfterFailure(t *testing.T) {
	repo := &fakeIdentityRepo{err: errors.New("db down")}
	var seen learner.Identity
	mw := RequireIdentity(fakeAuth{id: learner.Identity{ID: "dev", DisplayName: "Dev"}}, repo)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = IdentityFrom(r.Context())
	}))

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("GET", "/", nil))
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("first request code=%d, want 500", rec1.Code)
	}

	repo.err = nil // backing store recovers
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request code=%d, want 200", rec2.Code)
	}
	if seen.ID != "dev" {
		t.Fatalf("seen=%+v, want identity injected", seen)
	}
	if repo.upserts != 2 {
		t.Fatalf("upserts=%d, want 2 (retried after prior failure, not poisoned)", repo.upserts)
	}
}

// interactiveAuth is a stand-in for the oidc adapter: an authenticator
// that owns its own login flow (auth.Interactive). The point of these
// tests is the BRANCH, not the flow — that an unauthenticated browser
// navigation is handed to StartLogin instead of being dead-ended with
// a 401, and that everything else still gets the 401 it always did.
type interactiveAuth struct {
	fakeAuth
	started   int
	keptAlive int
}

func (i *interactiveAuth) StartLogin(w http.ResponseWriter, r *http.Request) {
	i.started++
	http.Redirect(w, r, "/auth/login", http.StatusFound)
}

func (i *interactiveAuth) KeepAlive(http.ResponseWriter, *http.Request) { i.keptAlive++ }

func TestRequireIdentitySendsBrowserNavigationsToLogin(t *testing.T) {
	a := &interactiveAuth{fakeAuth: fakeAuth{err: errors.New("no session")}}
	mw := RequireIdentity(a, &fakeIdentityRepo{})
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", "/sessions", nil))
	if rec.Code != http.StatusFound || a.started != 1 {
		t.Fatalf("code=%d started=%d, want a 302 into the login flow", rec.Code, a.started)
	}
}

func TestRequireIdentityStill401sWhereALoginCannotHelp(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		// A JSON client wants the 401 the API contract promises, not an
		// HTML login page it would follow and fail to parse.
		{"api GET", "GET", "/api/v1/sessions"},
		{"api POST", "POST", "/api/v1/words"},
		// A non-htmx form POST can't be replayed after the round trip
		// through the provider, so sending it to a login would lose the
		// submission silently.
		{"plain POST", "POST", "/sessions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &interactiveAuth{fakeAuth: fakeAuth{err: errors.New("no session")}}
			mw := RequireIdentity(a, &fakeIdentityRepo{})
			rec := httptest.NewRecorder()
			mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
				ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusUnauthorized || a.started != 0 {
				t.Fatalf("code=%d started=%d, want 401 and no login", rec.Code, a.started)
			}
		})
	}
}

// An htmx request qualifies whatever its method: the learner is on a
// live page, and the authenticator answers it with HX-Redirect rather
// than a 302 the XHR would follow into a cross-origin dead end.
func TestRequireIdentitySendsHtmxRequestsToLogin(t *testing.T) {
	a := &interactiveAuth{fakeAuth: fakeAuth{err: errors.New("no session")}}
	mw := RequireIdentity(a, &fakeIdentityRepo{})
	req := httptest.NewRequest("POST", "/sessions/1/feedback", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	if a.started != 1 {
		t.Fatalf("started=%d, want the htmx request handed to StartLogin", a.started)
	}
}

func TestRequireIdentityKeepsTheSessionAlive(t *testing.T) {
	a := &interactiveAuth{fakeAuth: fakeAuth{id: learner.Identity{ID: "sub-1"}}}
	mw := RequireIdentity(a, &fakeIdentityRepo{})
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if a.keptAlive != 1 {
		t.Fatalf("keptAlive=%d, want 1", a.keptAlive)
	}
}

// Static and authelia mode do not implement auth.Interactive, and must
// behave exactly as they did before it existed: a bare 401, no
// redirect, whatever the request looks like. This is the regression
// guard for `make up` and every test in this repo.
func TestNonInteractiveAuthenticatorsAreUnchanged(t *testing.T) {
	mw := RequireIdentity(fakeAuth{err: errors.New("nope")}, &fakeIdentityRepo{})
	for _, target := range []string{"/", "/sessions", "/api/v1/sessions"} {
		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
			ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code=%d, want 401", target, rec.Code)
		}
		if rec.Header().Get("Location") != "" || rec.Header().Get("HX-Redirect") != "" {
			t.Errorf("%s: a non-interactive authenticator produced a redirect", target)
		}
	}
}
