package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// APIToken is one credential belonging to one non-browser client — a
// reader app, the A2A chat server, the browser extension.
//
// The plaintext token appears nowhere in this struct, and there is no
// field it could be recovered from: it is shown once, at creation, and
// only its hash is ever persisted. See migration 00027.
type APIToken struct {
	ID         string
	Name       string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// APITokenGrant is what authenticating a token yields: who the request
// acts as, and what it is allowed to do. Both come from the stored row,
// never from the request.
type APITokenGrant struct {
	ID       string
	Identity learner.IdentityID
	Scopes   []string
}

type APITokenRepository interface {
	// Insert stores a new token. hash is the hex SHA-256 of the
	// plaintext; the plaintext is never passed to storage.
	Insert(ctx context.Context, id string, identity learner.IdentityID, name, hash string, scopes []string) error
	// LookupLive resolves a hash to its grant, ignoring revoked tokens.
	// It returns ErrNotFound when no live token has that hash — which is
	// also what an attacker guessing hashes gets.
	LookupLive(ctx context.Context, hash string) (APITokenGrant, error)
	// TouchLastUsed records that a token authenticated a request, so an
	// unused token is visibly unused when deciding what to revoke.
	TouchLastUsed(ctx context.Context, id string) error
	// List returns a learner's own tokens, revoked ones included.
	List(ctx context.Context, identity learner.IdentityID) ([]APIToken, error)
	// Revoke switches a token off. It reports whether a live token was
	// actually revoked, so a caller can tell "done" from "that token is
	// not yours or was already revoked" without leaking which.
	Revoke(ctx context.Context, id string, identity learner.IdentityID) (bool, error)
}
