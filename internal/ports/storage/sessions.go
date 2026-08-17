package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// SessionRepository persists writing sessions. Every method is
// identity-scoped, and every read hides soft-deleted sessions: Get and
// List apply "deleted_at IS NULL" in SQL (db/queries/sessions.sql), not
// in Go and not in a template, so no caller can forget — and neither
// can the documents, feedback requests, corrections and conversation
// turns hanging off a session, whose own queries test this row's
// deleted_at through an EXISTS sub-select. See
// internal/adapters/postgres/softdelete_guard_test.go for the test that
// enforces it.
type SessionRepository interface {
	Create(ctx context.Context, s session.Session) error
	Get(ctx context.Context, identity learner.IdentityID, id session.ID) (session.Session, error)
	List(ctx context.Context, identity learner.IdentityID) ([]session.Session, error)
	// SoftDelete hides id from every list, page, JSON API, agent tool
	// and export, along with everything hanging off it. Nothing is
	// erased: the learning_events behind the session stay, so /learner
	// and /outcomes are unchanged, and Restore below undoes it.
	//
	// Identity-scoped from the request context, never from the request
	// body: a session belonging to another identity returns ErrNotFound
	// — byte-for-byte the response an id that does not exist gets, so
	// there is no existence oracle — and is left completely untouched.
	//
	// Idempotent: deleting an already-deleted session is a success, not
	// an error, and keeps the original deletion timestamp.
	SoftDelete(ctx context.Context, identity learner.IdentityID, id session.ID, at time.Time) error
	// Restore is the way back from SoftDelete: the session and
	// everything hanging off it become visible again. Same
	// identity-scoping and same idempotence — restoring a session that
	// was never deleted is a success.
	Restore(ctx context.Context, identity learner.IdentityID, id session.ID) error
}
