package auth

import (
	"errors"
	"net/http"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type Authenticator interface {
	Authenticate(r *http.Request) (learner.Identity, error)
}
