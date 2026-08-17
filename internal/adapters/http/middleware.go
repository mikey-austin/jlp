package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type ctxKey int

const identityKey ctxKey = 1

func IdentityFrom(ctx context.Context) (learner.Identity, bool) {
	id, ok := ctx.Value(identityKey).(learner.Identity)
	return id, ok
}

func RequireIdentity(a auth.Authenticator, repo storage.IdentityRepository) func(http.Handler) http.Handler {
	var seen sync.Map
	// interactive is non-nil only when the configured authenticator
	// runs its own login flow (internal/adapters/oidc). In static and
	// authelia mode it stays nil and every branch below that mentions
	// it is dead code, which is what keeps those two modes behaving
	// exactly as they did before OIDC existed.
	interactive, _ := a.(auth.Interactive)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err != nil {
				if interactive != nil && wantsLogin(r) {
					interactive.StartLogin(w, r)
					return
				}
				requireIdentityError(w, r, http.StatusUnauthorized, "unauthorized")
				return
			}
			if interactive != nil {
				// Slide the idle deadline forward before the handler
				// runs — after it, the response may already be written.
				interactive.KeepAlive(w, r)
			}
			if _, ok := seen.Load(id.ID); !ok {
				if err := repo.Upsert(r.Context(), id); err != nil {
					requireIdentityError(w, r, http.StatusInternalServerError, "identity error")
					return
				}
				seen.Store(id.ID, true)
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
		})
	}
}

// wantsLogin reports whether an unauthenticated request should be sent
// to a login flow rather than told "401" — i.e. whether there is a
// human at a browser on the other end who could actually complete one.
//
//   - An htmx request always qualifies, whatever its method: it comes
//     from a page the learner is looking at right now, and the
//     authenticator answers it with HX-Redirect so the whole tab
//     navigates instead of a fragment being swapped with a login page.
//   - Otherwise only a plain GET/HEAD navigation outside /api/. A POST
//     cannot be replayed after login, and an /api/ client wants the
//     JSON 401 the contract promises it, not a 302 into an HTML login
//     page it would follow and then fail to parse.
func wantsLogin(r *http.Request) bool {
	if r.Header.Get("HX-Request") != "" {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// requireIdentityError writes msg as the middleware's error response:
// {"error":"..."} JSON for API callers (paths under /api/, per the
// Task 16 contract — a JSON client shouldn't have to sniff a plain-text
// body to detect auth failure), or the plain-text body every other
// route already returns via http.Error.
func requireIdentityError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
			slog.Error("write auth error response", "err", err)
		}
		return
	}
	http.Error(w, msg, status)
}
