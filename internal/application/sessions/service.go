package sessions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Service struct {
	repo storage.SessionRepository
}

func NewService(repo storage.SessionRepository) *Service {
	return &Service{repo: repo}
}

// Create validates title is non-empty, applies profile defaults, generates a
// uuid, and persists the new session.
func (s *Service) Create(ctx context.Context, identity learner.IdentityID, title, purpose string, p session.Profile) (session.Session, error) {
	if title == "" {
		return session.Session{}, errors.New("title must not be empty")
	}
	if p.TeacherMode == "" {
		p.TeacherMode = "teacher"
	}
	if p.ExplanationLanguage == "" {
		p.ExplanationLanguage = "both"
	}
	if p.Strictness == "" {
		p.Strictness = "balanced"
	}

	now := time.Now().UTC()
	sess := session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identity,
		Title:      title,
		Purpose:    purpose,
		Profile:    p,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.repo.Create(ctx, sess); err != nil {
		return session.Session{}, err
	}
	return sess, nil
}

func (s *Service) List(ctx context.Context, identity learner.IdentityID) ([]session.Session, error) {
	return s.repo.List(ctx, identity)
}

func (s *Service) Get(ctx context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	return s.repo.Get(ctx, identity, id)
}
