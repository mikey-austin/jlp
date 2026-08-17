package httpx

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// APIAuth authenticates the non-browser surfaces — /api/v1 and /a2a —
// where a session cookie may not exist.
//
// It is deliberately NOT RequireIdentity. Those routes have two kinds of
// caller with different needs, and one middleware that says so plainly
// beats one that infers it:
//
//   - A browser session (the learner clicking around, or the extension
//     riding its cookie) authenticates exactly as everywhere else, by
//     delegating to the configured Authenticator.
//   - A reader app, the A2A chat server, or anything else without a
//     browser presents an API token and gets what that token was minted
//     for — nothing more.
//
// Scope is enforced here rather than in each handler, and by
// default-deny: a token reaching a route no scope covers is refused even
// though the credential is perfectly valid. That is the same posture
// config.Channels.AllowFrom takes at the other untrusted edge, and it
// means adding a route cannot silently widen an existing token.
func APIAuth(tokens *apitoken.Service, session auth.Authenticator, repo storage.IdentityRepository) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// An Authorization header of ANY shape means the caller
			// intends to authenticate as a client, so a malformed one is
			// refused rather than quietly falling back to the session.
			// Falling back would make a client with a broken token config
			// appear to work whenever a session cookie happened to be
			// along for the ride, and fail everywhere else.
			if r.Header.Get("Authorization") == "" {
				// No credential of our own: this is the session path, and
				// it must behave exactly as it did before tokens existed.
				id, err := session.Authenticate(r)
				if err != nil {
					writeAPIError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				if err := repo.Upsert(r.Context(), id); err != nil {
					writeAPIError(w, http.StatusInternalServerError, "identity error")
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
				return
			}

			cred, ok := apiCredential(r)
			if !ok {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			if tokens == nil {
				// No token service wired up (no database). Say the
				// credential is bad rather than that the feature is off:
				// an unauthenticated caller learns nothing either way.
				writeAPIError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			grant, err := tokens.Verify(r.Context(), cred)
			if err != nil {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized")
				return
			}

			want, covered := requiredScope(r)
			if !covered || !apitoken.HasScope(grant, want) {
				// 403, not 401: the credential IS valid, so inviting the
				// client to retry with different credentials would be a
				// lie. Logged because a token hitting a route it cannot
				// use is either a misconfigured client or someone
				// probing, and both are worth seeing.
				slog.Warn("api: token lacks scope for route",
					"token_id", grant.ID, "identity", grant.Identity,
					"method", r.Method, "path", r.URL.Path,
					"want_scope", want, "route_covered", covered, "have", grant.Scopes)
				writeAPIError(w, http.StatusForbidden, "this token is not allowed to do that")
				return
			}

			// The identity comes from the stored token row, never from
			// anything the client sent. Look it up so handlers see the
			// same Identity a browser request would carry; a token's
			// identity_id is a foreign key, so it exists by construction.
			id, err := repo.Get(r.Context(), grant.Identity)
			if err != nil {
				slog.Error("api: token identity missing", "identity", grant.Identity, "err", err)
				writeAPIError(w, http.StatusInternalServerError, "identity error")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
		})
	}
}

// apiCredential extracts a token from the Authorization header.
//
// Bearer is the normal form. Basic is supported because some clients —
// reader apps in particular — offer only a URL field, and
// https://reader:<token>@jlp.lan/... is the one way to attach a
// credential to a bare URL. Both halves are accepted as the token so
// that https://<token>@host/ works too, which is what a client that
// takes "username only" produces.
//
// A URL-embedded credential is a real trade: it ends up in logs, crash
// reports and settings backups. It is offered anyway because the
// alternative those clients force is a single shared password, and a
// per-app token that can be revoked on its own is strictly better than
// that even when it travels badly.
func apiCredential(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, rest, found := strings.Cut(h, " ")
	if !found {
		return "", false
	}
	switch strings.ToLower(scheme) {
	case "bearer":
		rest = strings.TrimSpace(rest)
		return rest, rest != ""
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "", false
		}
		user, pass, _ := strings.Cut(string(raw), ":")
		if pass != "" {
			return pass, true
		}
		return user, user != ""
	}
	return "", false
}

// requiredScope maps a request to the scope a token must carry, and
// reports whether any scope covers it at all.
//
// An uncovered route is refused for token callers. That is the whole
// point: /api/v1 also serves sessions, feedback and learner statistics,
// and a token minted so a reader app can add words has no business
// reading a learner's correction history.
func requiredScope(r *http.Request) (string, bool) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/api/v1/words":
		return apitoken.ScopeVocabularyWrite, true
	case r.Method == http.MethodPost && p == "/api/v1/vocabulary/events":
		return apitoken.ScopeVocabularyWrite, true
	case strings.HasPrefix(p, "/a2a"):
		return apitoken.ScopeA2AUse, true
	}
	return "", false
}
