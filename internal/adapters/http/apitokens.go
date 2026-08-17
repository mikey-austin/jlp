package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// The API-token management UI, under /settings/tokens.
//
// It lives on the settings page rather than getting a nav item of its
// own for the same reason the model settings do: minting a credential is
// something a learner does once per app, not part of daily use.

// apiTokensPage handles GET /settings/tokens.
//
// The freshly minted token, when there is one, arrives via the redirect
// as a query parameter and is shown exactly once. It is deliberately
// NOT stored anywhere to be re-displayed: the plaintext exists only in
// this response, which is the property that makes a leaked database
// backup useless.
func (s *Server) apiTokensPage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	var tokens []storage.APIToken
	var listErr string
	if s.opts.APITokens != nil {
		var err error
		tokens, err = s.opts.APITokens.List(r.Context(), ident.ID)
		if err != nil {
			slog.Error("api tokens: list", "err", err, "identity", ident.ID)
			listErr = "トークンを読み込めませんでした。"
		}
	}

	s.render(w, r, "tokens", map[string]any{
		"Title":    "APIトークン",
		"Identity": ident,
		"Tokens":   tokens,
		"Scopes":   apitoken.Scopes,
		"Error":    firstNonEmpty(r.URL.Query().Get("error"), listErr),
		// Shown once, then gone on the next navigation.
		"NewToken": r.URL.Query().Get("token"),
		"Enabled":  s.opts.APITokens != nil,
	})
}

// apiTokensCreate handles POST /settings/tokens.
func (s *Server) apiTokensCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if s.opts.APITokens == nil {
		http.Redirect(w, r, "/settings/tokens?error="+url.QueryEscape("トークンは利用できません。"), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/tokens?error="+url.QueryEscape("フォームを読み取れませんでした。"), http.StatusSeeOther)
		return
	}

	plaintext, err := s.opts.APITokens.Mint(r.Context(), ident.ID, r.FormValue("name"), r.Form["scopes"])
	switch {
	case errors.Is(err, apitoken.ErrUnknownScope):
		http.Redirect(w, r, "/settings/tokens?error="+url.QueryEscape("不明なスコープです。"), http.StatusSeeOther)
		return
	case err != nil:
		slog.Error("api tokens: mint", "err", err, "identity", ident.ID)
		http.Redirect(w, r, "/settings/tokens?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	// The plaintext travels in the redirect URL, which puts it in this
	// browser's history. That is a real cost, accepted because the
	// alternatives are worse: re-rendering on POST breaks refresh, and
	// stashing it server-side to show later would mean storing the one
	// thing this design exists to never store. It is shown once and the
	// learner is told to copy it now.
	http.Redirect(w, r, "/settings/tokens?token="+url.QueryEscape(plaintext), http.StatusSeeOther)
}

// apiTokensRevoke handles POST /settings/tokens/{id}/revoke.
func (s *Server) apiTokensRevoke(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if s.opts.APITokens == nil {
		http.Redirect(w, r, "/settings/tokens", http.StatusSeeOther)
		return
	}
	// Revoke is identity-scoped in SQL, so a token id belonging to
	// someone else revokes nothing and reports nothing — the caller
	// cannot use this to discover whether an id exists.
	if _, err := s.opts.APITokens.Revoke(r.Context(), chi.URLParam(r, "id"), ident.ID); err != nil {
		slog.Error("api tokens: revoke", "err", err, "identity", ident.ID)
		http.Redirect(w, r, "/settings/tokens?error="+url.QueryEscape("取り消せませんでした。"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings/tokens", http.StatusSeeOther)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
