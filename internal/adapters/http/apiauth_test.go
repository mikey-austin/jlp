package httpx

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// tokenRepoStub is the storage side of one minted token.
type tokenRepoStub struct {
	hash     string
	identity learner.IdentityID
	scopes   []string
	revoked  bool
}

func (s *tokenRepoStub) Insert(context.Context, string, learner.IdentityID, string, string, []string) error {
	return nil
}

func (s *tokenRepoStub) LookupLive(_ context.Context, hash string) (storage.APITokenGrant, error) {
	if hash != s.hash || s.revoked {
		return storage.APITokenGrant{}, storage.ErrNotFound
	}
	return storage.APITokenGrant{ID: "tok-1", Identity: s.identity, Scopes: s.scopes}, nil
}
func (s *tokenRepoStub) TouchLastUsed(context.Context, string) error { return nil }
func (s *tokenRepoStub) List(context.Context, learner.IdentityID) ([]storage.APIToken, error) {
	return nil, nil
}
func (s *tokenRepoStub) Revoke(context.Context, string, learner.IdentityID) (bool, error) {
	return true, nil
}

// apiAuthFixture wires APIAuth over a handler that reports which
// identity reached it, so a test can assert not just "allowed" but
// "allowed AS whom".
func apiAuthFixture(t *testing.T, plaintext string, scopes []string) http.Handler {
	t.Helper()
	repo := &tokenRepoStub{hash: apitoken.Hash(plaintext), identity: "reader-owner", scopes: scopes}
	mw := APIAuth(apitoken.New(repo), testAuth{id: learner.Identity{ID: "session-user"}}, testIdentityRepo{})
	return mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := IdentityFrom(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(id.ID))
	}))
}

func TestTokenAuthenticatesAsItsOwnIdentity(t *testing.T) {
	h := apiAuthFixture(t, "jlp_good", []string{apitoken.ScopeVocabularyWrite})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/words", nil)
	req.Header.Set("Authorization", "Bearer jlp_good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	// The identity must come from the stored row, not from the session
	// authenticator that would otherwise have answered.
	if got := rec.Body.String(); got != "reader-owner" {
		t.Errorf("acted as %q, want reader-owner — a token must act as the learner it was minted for", got)
	}
}

// A token minted for one job must not do another. This is the guarantee
// that makes handing a reader app a credential reasonable at all.
func TestTokenCannotExceedItsScope(t *testing.T) {
	h := apiAuthFixture(t, "jlp_good", []string{apitoken.ScopeVocabularyWrite})

	for _, path := range []string{
		"/a2a/",                      // covered by a scope this token lacks
		"/api/v1/learner/statistics", // covered by no scope at all
		"/api/v1/sessions",           // covered by no scope at all
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer jlp_good")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a vocabulary:write token = %d, want 403 (body %s)", path, rec.Code, rec.Body)
		}
	}
}

// 401 invites a retry with different credentials; 403 says these
// credentials are real but not enough. Conflating them tells a client
// the wrong thing to do.
func TestBadCredentialsAre401AndInsufficientAre403(t *testing.T) {
	h := apiAuthFixture(t, "jlp_good", []string{apitoken.ScopeVocabularyWrite})

	cases := []struct {
		name, header string
		want         int
	}{
		{"unknown token", "Bearer jlp_wrong", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"valid token, unscoped route", "Bearer jlp_good", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := "/api/v1/sessions"
			if c.want == http.StatusUnauthorized {
				path = "/api/v1/words"
			}
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Authorization", c.header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body)
			}
		})
	}
}

// The URL-credential form is the whole reason Basic is accepted: a
// reader app whose only setting is a URL must still be able to
// authenticate.
func TestBasicAuthCarriesTheToken(t *testing.T) {
	h := apiAuthFixture(t, "jlp_good", []string{apitoken.ScopeVocabularyWrite})

	for _, creds := range []string{
		"reader:jlp_good", // https://reader:<token>@host/...
		"jlp_good:",       // https://<token>@host/...
		"jlp_good",        // no colon at all
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/words", nil)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Basic %q = %d, want 200 (body %s)", creds, rec.Code, rec.Body)
		}
	}
}

// With no credential of its own, the API surface must behave exactly as
// it did before tokens existed.
func TestNoAuthorizationHeaderFallsBackToTheSession(t *testing.T) {
	h := apiAuthFixture(t, "jlp_good", []string{apitoken.ScopeVocabularyWrite})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/words", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "session-user" {
		t.Errorf("acted as %q, want session-user", got)
	}
}

// A revoked token must stop working at the middleware, not just in the
// UI listing.
func TestRevokedTokenIsRefusedByTheMiddleware(t *testing.T) {
	repo := &tokenRepoStub{hash: apitoken.Hash("jlp_good"), identity: "reader-owner", scopes: []string{apitoken.ScopeVocabularyWrite}, revoked: true}
	mw := APIAuth(apitoken.New(repo), testAuth{id: learner.Identity{ID: "session-user"}}, testIdentityRepo{})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/words", nil)
	req.Header.Set("Authorization", "Bearer jlp_good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d, want 401", rec.Code)
	}
}

// The routing guarantee: tokens are accepted ONLY on the API group. A
// token must never open an HTML page, whatever scope it carries — that
// is what keeps "this app can add words" from meaning "this app can read
// my correction history".
func TestTokensDoNotOpenTheHTMLRoutes(t *testing.T) {
	opts := testOptions()
	// No session: a browser request would be told to log in, and the only
	// credential offered below is a token.
	opts.Auth = failingAuth{}
	opts.APITokens = apitoken.New(&tokenRepoStub{
		hash:     apitoken.Hash("jlp_good"),
		identity: "reader-owner",
		scopes:   []string{apitoken.ScopeVocabularyWrite, apitoken.ScopeA2AUse},
	})
	h := NewServer(opts).HandlerForTest()

	for _, path := range []string{"/", "/sessions", "/vocabulary", "/settings", "/settings/tokens"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer jlp_good")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Errorf("GET %s with an API token = 200; a token must not reach the HTML routes", path)
		}
	}
}
