package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// Each of the extension's actions needs its OWN scope, and a token
// carrying one must not be able to perform another. The point of
// splitting sessions:write from feedback:request is that parking text
// costs nothing while asking for corrections spends money on a model
// call — a token allowed only to file things away must not be able to
// run up a bill.
func TestExtensionScopesDoNotSubstituteForEachOther(t *testing.T) {
	cases := []struct {
		name, scope, method, path string
		want                      int
	}{
		{"create a session with sessions:write", apitoken.ScopeSessionsWrite, http.MethodPost, "/api/v1/sessions", http.StatusOK},
		{"create a session with only feedback:request", apitoken.ScopeFeedbackRequest, http.MethodPost, "/api/v1/sessions", http.StatusForbidden},
		{"request feedback with only sessions:write", apitoken.ScopeSessionsWrite, http.MethodPost, "/api/v1/sessions/x/feedback", http.StatusForbidden},
		{"save a word with only sessions:write", apitoken.ScopeSessionsWrite, http.MethodPost, "/api/v1/words", http.StatusForbidden},
		{"a2a with only sessions:write", apitoken.ScopeSessionsWrite, http.MethodPost, "/a2a/", http.StatusForbidden},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &tokenRepoStub{hash: apitoken.Hash("jlp_tok"), identity: "dev", scopes: []string{c.scope}}
			mw := APIAuth(apitoken.New(repo), testAuth{id: learner.Identity{ID: "session-user"}}, testIdentityRepo{})
			h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

			req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer jlp_tok")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
			}
		})
	}
}

// The scopes must be mintable, or the settings page cannot offer them
// and the extension can never be given one.
func TestNewScopesAreMintable(t *testing.T) {
	for _, scope := range []string{apitoken.ScopeSessionsWrite, apitoken.ScopeFeedbackRequest} {
		var found bool
		for _, s := range apitoken.Scopes {
			if s == scope {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is enforced on a route but is not in apitoken.Scopes, so no token can ever carry it", scope)
		}
	}
}
