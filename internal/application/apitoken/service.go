// Package apitoken issues and verifies credentials for non-browser
// clients: reader apps that upload words, the A2A chat server, the
// browser extension.
//
// It exists because OIDC has no answer for them. A reader app has no
// browser to run a login flow in, so under APP_AUTH_MODE=oidc every
// request it makes gets a redirect it cannot follow. The alternative
// people reach for — one shared username and password in a URL — is a
// single unscoped secret that cannot be revoked for one app without
// breaking all of them, and that ends up in logs, crash reports and
// settings backups.
//
// The model here follows config.Channels.AllowFrom, which solved the
// same problem for inbound chat messages: this is an untrusted edge, so
// a credential is bound to exactly one learner identity and grants
// exactly the scopes it was minted with. Nothing is inferred from the
// request.
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ScopeVocabularyWrite lets a client add words. It is the only scope
// today; the set is a closed list rather than free text so that a typo
// in the minting form produces a token that grants nothing, instead of a
// token that grants something nobody named.
const ScopeVocabularyWrite = "vocabulary:write"

// ScopeA2AUse lets a client drive the A2A adapter — today, the chat
// server in clients/a2a-chat. It is separate from vocabulary:write and
// far broader: A2A skills reach the learner's own material, so a reader
// app that only uploads words must never be handed this by default.
const ScopeA2AUse = "a2a:use"

// ScopeSessionsWrite lets a client create a session, optionally with
// the text it is about — the browser extension's "park this selection
// for later".
const ScopeSessionsWrite = "sessions:write"

// ScopeFeedbackRequest lets a client ask for corrections on a piece of
// text.
//
// Separate from sessions:write rather than folded into one
// "writing:review" scope, because the two grants differ in a way worth
// being able to withhold: parking text costs nothing, while every
// feedback request spends real money on a model call. A client that
// only files things away should not be able to run up a bill.
const ScopeFeedbackRequest = "feedback:request"

// ScopeReadingWrite lets a client send an article to the 読解 pipeline,
// follow its study edition's progress, and ask for it to be sent to the
// Kindle — the Chrome extension's 「JLPでKindle版を作成」. It is its own
// scope, separate from sessions:write, because every submission queues
// a model call: parking text costs nothing, a study edition costs money.
// It grants no read of anything else, and the status it can read is
// only that of editions the token's own identity owns.
const ScopeReadingWrite = "reading:write"

// Scopes is every scope a token may be minted with.
var Scopes = []string{ScopeVocabularyWrite, ScopeSessionsWrite, ScopeFeedbackRequest, ScopeReadingWrite, ScopeA2AUse}

// tokenPrefix marks a JLP credential wherever it turns up — a log line,
// a config file, a secret scanner's ruleset.
const tokenPrefix = "jlp_"

// ErrUnknownScope is returned when minting is asked for a scope that is
// not in Scopes.
var ErrUnknownScope = errors.New("apitoken: unknown scope")

// ErrInvalid is returned by Verify for every failure: no such token,
// revoked, malformed. One error, so a caller cannot accidentally tell a
// client which of those it was.
var ErrInvalid = errors.New("apitoken: invalid token")

type Service struct{ repo storage.APITokenRepository }

func New(repo storage.APITokenRepository) *Service { return &Service{repo: repo} }

// Mint creates a token and returns its plaintext EXACTLY ONCE. Nothing
// stores the plaintext, so a caller that discards this string has
// created a token nobody can ever use — which is the correct trade
// against a database backup yielding working credentials.
func (s *Service) Mint(ctx context.Context, identity learner.IdentityID, name string, scopes []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("apitoken: a token needs a name, so it can be revoked by knowing which app it belongs to")
	}
	for _, sc := range scopes {
		if !slicesContains(Scopes, sc) {
			return "", fmt.Errorf("%w: %q", ErrUnknownScope, sc)
		}
	}

	// 32 bytes of crypto/rand: far past guessing, and the reason a plain
	// SHA-256 is the right store for it. There is no dictionary to attack
	// and no work factor worth paying, unlike a human-chosen password.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("apitoken: generate: %w", err)
	}
	plaintext := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	if err := s.repo.Insert(ctx, uuid.NewString(), identity, name, Hash(plaintext), scopes); err != nil {
		return "", fmt.Errorf("apitoken: store: %w", err)
	}
	return plaintext, nil
}

// Verify resolves a presented token to the identity it acts as and the
// scopes it carries, and records the use.
func (s *Service) Verify(ctx context.Context, presented string) (storage.APITokenGrant, error) {
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return storage.APITokenGrant{}, ErrInvalid
	}
	grant, err := s.repo.LookupLive(ctx, Hash(presented))
	if errors.Is(err, storage.ErrNotFound) {
		return storage.APITokenGrant{}, ErrInvalid
	}
	if err != nil {
		return storage.APITokenGrant{}, err
	}
	// Best-effort: a client whose request worked must not be failed
	// because a bookkeeping write did. The caller has already been
	// authenticated by this point.
	_ = s.repo.TouchLastUsed(ctx, grant.ID)
	return grant, nil
}

func (s *Service) List(ctx context.Context, identity learner.IdentityID) ([]storage.APIToken, error) {
	return s.repo.List(ctx, identity)
}

func (s *Service) Revoke(ctx context.Context, id string, identity learner.IdentityID) (bool, error) {
	return s.repo.Revoke(ctx, id, identity)
}

// Hash is how a plaintext token becomes the value stored and looked up.
// Exported so tests can construct a stored row without going through
// Mint, and so there is exactly one definition of it.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// HasScope reports whether a grant carries scope. Callers must ask
// before acting: authentication says who, never what.
func HasScope(grant storage.APITokenGrant, scope string) bool {
	return slicesContains(grant.Scopes, scope)
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
