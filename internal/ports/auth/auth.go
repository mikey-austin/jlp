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

// Interactive is the OPTIONAL extension an Authenticator implements
// when it owns the browser session itself — it runs a login flow and
// issues its own cookie — instead of trusting an identity an upstream
// proxy already established (internal/adapters/authelia) or being
// handed one at construction (internal/adapters/staticauth).
//
// It exists because those two postures need different behaviour from
// the SAME middleware on the SAME failure. With a forward-auth proxy
// in front, an unauthenticated request never reaches JLP at all: the
// proxy has already sent the browser to the login page, so a bare 401
// is the correct (and unreachable) response. When JLP authenticates
// for itself, a 401 on a browser navigation is a dead end — nothing
// else is going to start the login. So internal/adapters/http's
// RequireIdentity type-asserts for this interface and, when it's
// present, hands a browser navigation to StartLogin instead of writing
// a 401. Adapters that don't implement it keep exactly their previous
// behaviour, which is why `static` and `authelia` mode are untouched
// by OIDC existing.
//
// Both methods take the ResponseWriter that Authenticate deliberately
// does not: Authenticate answers "who is this request" and must stay
// side-effect-free (it's called from the A2A bridge and any future
// non-HTTP caller), while starting a login and extending a session are
// both response-writing acts.
type Interactive interface {
	Authenticator
	// StartLogin begins an interactive login for an unauthenticated
	// request, arranging for the browser to come back to what it was
	// asking for. It writes the whole response; the caller must not
	// write anything else afterwards.
	StartLogin(w http.ResponseWriter, r *http.Request)
	// KeepAlive extends the idle deadline of the session that just
	// authenticated r, if the implementation has an idle deadline at
	// all. It is called only AFTER a successful Authenticate, and must
	// be a no-op (writing nothing) when there is nothing to extend —
	// it runs before the wrapped handler, so an unwanted write here
	// would corrupt every response.
	KeepAlive(w http.ResponseWriter, r *http.Request)
}
