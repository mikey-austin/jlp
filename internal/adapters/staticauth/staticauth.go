package staticauth

import (
	"net/http"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

type Static struct{ identity learner.Identity }

func New(id, displayName string) auth.Authenticator {
	return Static{identity: learner.Identity{ID: learner.IdentityID(id), DisplayName: displayName}}
}

func (s Static) Authenticate(*http.Request) (learner.Identity, error) { return s.identity, nil }
