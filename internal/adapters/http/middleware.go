package httpx

import (
	"context"
	"net/http"
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
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if _, ok := seen.Load(id.ID); !ok {
				if err := repo.Upsert(r.Context(), id); err != nil {
					http.Error(w, "identity error", http.StatusInternalServerError)
					return
				}
				seen.Store(id.ID, true)
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
		})
	}
}
