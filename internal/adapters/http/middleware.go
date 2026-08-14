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
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := a.Authenticate(r)
			if err != nil {
				requireIdentityError(w, r, http.StatusUnauthorized, "unauthorized")
				return
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
