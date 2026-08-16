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

// ErrInvalidTitle is returned by Create when title is empty. It's a
// sentinel (rather than a plain errors.New at the call site) so HTTP
// handlers can distinguish this validation failure — safe to surface to
// the caller verbatim — from any other error Create or its repository
// returns, which must not be echoed back (see internal/adapters/http's
// sessionsCreate/apiSessionsCreate).
var ErrInvalidTitle = errors.New("title must not be empty")

// Create validates title is non-empty, applies profile defaults, generates a
// uuid, and persists the new session.
func (s *Service) Create(ctx context.Context, identity learner.IdentityID, title, purpose string, p session.Profile) (session.Session, error) {
	if title == "" {
		return session.Session{}, ErrInvalidTitle
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
	if p.FeedbackTiming == "" {
		// PRD §17.4: dialogue shouldn't be constantly interrupted by
		// correction UI — "end" (nothing shown until the conversation
		// tutor's Summarise digest) is the default every new session
		// gets unless the learner explicitly opts into "immediate" or
		// "delayed" at creation time.
		p.FeedbackTiming = "end"
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
