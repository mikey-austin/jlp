package writing

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Service struct {
	docs storage.DocumentRepository
}

func NewService(docs storage.DocumentRepository) *Service {
	return &Service{docs: docs}
}

// Open returns the session's document, creating an empty one on first
// visit. There is exactly one document per session.
func (s *Service) Open(ctx context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error) {
	return s.docs.GetOrCreateForSession(ctx, identity, sid)
}

// Autosave persists the editor's current content, bumping the document's
// version and recording a version-history row.
func (s *Service) Autosave(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	return s.docs.Save(ctx, identity, id, content)
}
