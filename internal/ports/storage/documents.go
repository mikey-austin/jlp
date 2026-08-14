package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
)

// DocumentRepository is identity-scoped: every method takes the caller's
// identity and returns storage.ErrNotFound rather than another identity's
// document, mirroring SessionRepository's access-control pattern.
type DocumentRepository interface {
	// GetOrCreateForSession returns the session's single document, creating
	// an empty one (version 1) the first time it's opened.
	GetOrCreateForSession(ctx context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error)
	// Save persists new content, incrementing Version and appending a
	// document_versions row in one transaction.
	Save(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error)
	Get(ctx context.Context, identity learner.IdentityID, id writing.DocumentID) (writing.Document, error)
	ListVersions(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, limit int) ([]writing.Document, error)
}
