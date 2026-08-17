package sessions

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Service struct {
	repo storage.SessionRepository
	rec  *learning.Recorder
}

func NewService(repo storage.SessionRepository, rec *learning.Recorder) *Service {
	return &Service{repo: repo, rec: rec}
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

// Delete soft-deletes id: the session, its document, its feedback
// history, its corrections and its conversation turns all disappear
// from every list, page, JSON API and agent tool. Nothing is erased —
// the learning_events behind the session stay, so /learner and
// /outcomes read exactly the same afterwards — and Restore below brings
// it all back.
//
// identity MUST come from the request context (httpx.IdentityFrom),
// never from a form field, a query parameter or a JSON body: it is the
// only thing standing between one learner and another's data, and this
// codebase has already had a client-forgeable id once (speech_event_id).
// A session belonging to someone else returns storage.ErrNotFound, the
// same error an unknown id returns, and is left untouched.
//
// Deleting an already-deleted session is a success, not an error.
//
// The content.deleted event is recorded after the delete has already
// taken effect, so a failure to record it is logged and swallowed
// rather than reported as a failed delete — the same log-and-continue
// tradeoff application/lessons.Service.Generate documents.
func (s *Service) Delete(ctx context.Context, identity learner.IdentityID, id session.ID) error {
	if err := s.repo.SoftDelete(ctx, identity, id, time.Now().UTC()); err != nil {
		return err
	}
	if err := s.rec.RecordDeletion(ctx, identity, learning.KindSession, string(id)); err != nil {
		slog.Error("record content.deleted", "identity", identity, "kind", "session", "subject", id, "err", err)
	}
	return nil
}

// Restore undoes Delete — the session and everything hanging off it
// become visible again. Same identity-from-context rule, same
// ErrNotFound-for-someone-else's-session contract, and restoring a
// session that was never deleted is a success.
func (s *Service) Restore(ctx context.Context, identity learner.IdentityID, id session.ID) error {
	if err := s.repo.Restore(ctx, identity, id); err != nil {
		return err
	}
	if err := s.rec.RecordRestore(ctx, identity, learning.KindSession, string(id)); err != nil {
		slog.Error("record content.restored", "identity", identity, "kind", "session", "subject", id, "err", err)
	}
	return nil
}
