package authelia

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

type Authelia struct{ trusted []netip.Prefix }

func New(cidrs []string) (auth.Authenticator, error) {
	a := Authelia{}
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("authelia: bad trusted proxy %q: %w", c, err)
		}
		a.trusted = append(a.trusted, p)
	}
	return a, nil
}

func (a Authelia) Authenticate(r *http.Request) (learner.Identity, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return learner.Identity{}, auth.ErrUnauthenticated
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return learner.Identity{}, auth.ErrUnauthenticated
	}
	ok := false
	for _, p := range a.trusted {
		if p.Contains(addr) {
			ok = true
			break
		}
	}
	user := r.Header.Get("Remote-User")
	if !ok || user == "" {
		return learner.Identity{}, auth.ErrUnauthenticated
	}
	name := r.Header.Get("Remote-Name")
	if name == "" {
		name = user
	}
	return learner.Identity{ID: learner.IdentityID(user), DisplayName: name}, nil
}
