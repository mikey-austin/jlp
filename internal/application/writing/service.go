package writing

import (
	"context"
	"log/slog"

	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Service struct {
	docs storage.DocumentRepository
	rec  *learning.Recorder
}

func NewService(docs storage.DocumentRepository, rec *learning.Recorder) *Service {
	return &Service{docs: docs, rec: rec}
}

// Open returns the session's document, creating an empty one on first
// visit. There is exactly one document per session. A writing.created
// event is recorded only when this call is the one that created it. A
// failure to record that event does not fail Open: the document exists
// either way, so a recording failure is logged and treated as an
// observability concern, not a document-creation failure.
func (s *Service) Open(ctx context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error) {
	doc, created, err := s.docs.GetOrCreateForSession(ctx, identity, sid)
	if err != nil {
		return writing.Document{}, err
	}
	if created {
		if err := s.rec.Record(ctx, event.LearningEvent{
			IdentityID: identity,
			SessionID:  &doc.SessionID,
			Type:       event.TypeWritingCreated,
			Subject:    string(doc.ID),
		}); err != nil {
			slog.Error("record writing.created", "identity", identity, "session", sid, "document", doc.ID, "err", err)
		}
	}
	return doc, nil
}

// Autosave persists the editor's current content, bumping the document's
// version and recording a version-history row, then records a
// writing.updated event with the resulting rune count and version. The
// content is already durably saved by the time the event is recorded, so
// a failure to record it does not fail Autosave: the caller's writing is
// safe either way, and the recording failure is logged and treated as an
// observability concern, not a save failure.
func (s *Service) Autosave(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	doc, err := s.docs.Save(ctx, identity, id, content)
	if err != nil {
		return writing.Document{}, err
	}
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		SessionID:  &doc.SessionID,
		Type:       event.TypeWritingUpdated,
		Subject:    string(doc.ID),
		Evidence:   map[string]any{"rune_count": doc.RuneCount(), "version": doc.Version},
	}); err != nil {
		slog.Error("record writing.updated", "identity", identity, "document", doc.ID, "err", err)
	}
	return doc, nil
}
