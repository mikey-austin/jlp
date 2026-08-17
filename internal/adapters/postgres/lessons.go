package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// LessonRepository persists tutor lesson guides and their post-lesson
// observations (Phase 3 Task 4, PRD §18/§60): the lessons and
// lesson_observations tables (00016 migration) — see
// storage.LessonRepository's doc comment for the full identity-scoping
// contract.
type LessonRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewLessonRepository(pool *pgxpool.Pool) *LessonRepository {
	return &LessonRepository{pool: pool, q: sqlcgen.New(pool)}
}

// Insert persists a newly generated lesson, Status "prepared".
func (r *LessonRepository) Insert(ctx context.Context, l storage.Lesson) error {
	id, err := parseUUID(l.ID)
	if err != nil {
		return fmt.Errorf("lesson id: %w", err)
	}
	return r.q.InsertLesson(ctx, sqlcgen.InsertLessonParams{
		ID:         id,
		IdentityID: string(l.IdentityID),
		Plan:       l.Plan,
		Status:     l.Status,
		CreatedAt:  pgtype.Timestamptz{Time: l.CreatedAt, Valid: true},
	})
}

// List returns identity's lessons, newest first.
func (r *LessonRepository) List(ctx context.Context, identity learner.IdentityID) ([]storage.Lesson, error) {
	rows, err := r.q.ListLessons(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.Lesson, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromLessonRow(row))
	}
	return out, nil
}

// Get reads back one lesson, identity-scoped: a wrong identity or
// unknown id both miss with storage.ErrNotFound.
func (r *LessonRepository) Get(ctx context.Context, identity learner.IdentityID, id string) (storage.Lesson, error) {
	lessonID, err := parseUUID(id)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lesson id: %w", err)
	}
	row, err := r.q.GetLesson(ctx, sqlcgen.GetLessonParams{ID: lessonID, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.Lesson{}, storage.ErrNotFound
		}
		return storage.Lesson{}, err
	}
	return fromLessonRow(row), nil
}

// CompleteWithObservation persists o AND sets its target lesson's
// Status to "completed" (CompletedAt = at), in ONE transaction — see
// storage.LessonRepository.CompleteWithObservation's doc comment for
// why this must be atomic rather than two independent calls (the
// code-review finding this replaced: a lesson left "completed" without
// its observation attached, or vice versa, is either invisible in the
// UI or duplicable on retry). CompleteLesson runs first (its own
// identity-scoped WHERE clause is what turns a wrong identity or
// unknown id into pgx.ErrNoRows / storage.ErrNotFound); the observation
// insert runs second, inside the SAME transaction, and independently
// re-checks lesson ownership via its own join (see db/queries/
// lessons.sql's InsertLessonObservation) — belt-and-suspenders, not
// load-bearing given CompleteLesson's row lock already guarantees the
// row exists and is owned by identity by the time this runs. Either
// statement failing (a wrong identity, an unknown lesson ID, or any
// other error — e.g. a duplicate observation ID) rolls back BOTH: the
// status flip is never left committed without its observation, and the
// observation is never left attached without the status flip.
func (r *LessonRepository) CompleteWithObservation(ctx context.Context, identity learner.IdentityID, lessonID string, o storage.LessonObservation, at time.Time) (storage.Lesson, error) {
	lid, err := parseUUID(lessonID)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lesson id: %w", err)
	}
	obsID, err := parseUUID(o.ID)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("observation id: %w", err)
	}
	obsLessonID, err := parseUUID(o.LessonID)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("observation lesson id: %w", err)
	}
	subjects := o.Subjects
	if subjects == nil {
		subjects = []string{}
	}
	subjectsJSON, err := json.Marshal(subjects)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("subjects: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return storage.Lesson{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded
	qtx := r.q.WithTx(tx)

	completed, err := qtx.CompleteLesson(ctx, sqlcgen.CompleteLessonParams{
		ID:          lid,
		IdentityID:  string(identity),
		CompletedAt: pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.Lesson{}, storage.ErrNotFound
		}
		return storage.Lesson{}, err
	}

	rows, err := qtx.InsertLessonObservation(ctx, sqlcgen.InsertLessonObservationParams{
		ID:         obsID,
		LessonID:   obsLessonID,
		Author:     o.Author,
		Notes:      o.Notes,
		Subjects:   subjectsJSON,
		CreatedAt:  pgtype.Timestamptz{Time: o.CreatedAt, Valid: true},
		IdentityID: string(identity),
	})
	if err != nil {
		return storage.Lesson{}, err
	}
	if rows == 0 {
		return storage.Lesson{}, storage.ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return storage.Lesson{}, err
	}
	return fromLessonRow(completed), nil
}

// Observations returns every observation recorded against lessonID,
// newest first, scoped via the same join CompleteWithObservation uses
// — see storage.LessonRepository.Observations' doc comment for the
// "filters, doesn't error" contract this implements.
func (r *LessonRepository) Observations(ctx context.Context, identity learner.IdentityID, lessonID string) ([]storage.LessonObservation, error) {
	id, err := parseUUID(lessonID)
	if err != nil {
		return nil, fmt.Errorf("lesson id: %w", err)
	}
	rows, err := r.q.ListLessonObservations(ctx, sqlcgen.ListLessonObservationsParams{
		LessonID:   id,
		IdentityID: string(identity),
	})
	if err != nil {
		return nil, err
	}
	out := make([]storage.LessonObservation, 0, len(rows))
	for _, row := range rows {
		obs, err := fromLessonObservationRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, obs)
	}
	return out, nil
}

// SoftDelete marks the lesson deleted — see
// storage.LessonRepository.SoftDelete for the contract. The rows-
// affected count is the whole authorization answer: SoftDeleteLesson's
// WHERE carries identity_id, so zero rows means "no such lesson for
// THIS identity", whether the id belongs to someone else or to nobody,
// and both come back as ErrNotFound. Mirrors SessionRepository.
// SoftDelete exactly, including mapping a malformed id to ErrNotFound
// rather than a distinguishable parse error.
func (r *LessonRepository) SoftDelete(ctx context.Context, identity learner.IdentityID, lessonID string, at time.Time) error {
	pgID, err := parseUUID(lessonID)
	if err != nil {
		return storage.ErrNotFound
	}
	rows, err := r.q.SoftDeleteLesson(ctx, sqlcgen.SoftDeleteLessonParams{
		ID:         pgID,
		IdentityID: string(identity),
		At:         pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// Restore clears the deletion mark — see
// storage.LessonRepository.Restore.
func (r *LessonRepository) Restore(ctx context.Context, identity learner.IdentityID, lessonID string) error {
	pgID, err := parseUUID(lessonID)
	if err != nil {
		return storage.ErrNotFound
	}
	rows, err := r.q.RestoreLesson(ctx, sqlcgen.RestoreLessonParams{
		ID:         pgID,
		IdentityID: string(identity),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func fromLessonRow(row sqlcgen.Lesson) storage.Lesson {
	return storage.Lesson{
		ID:          uuid.UUID(row.ID.Bytes).String(),
		IdentityID:  learner.IdentityID(row.IdentityID),
		Plan:        row.Plan,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt.Time,
		CompletedAt: row.CompletedAt.Time,
	}
}

func fromLessonObservationRow(row sqlcgen.LessonObservation) (storage.LessonObservation, error) {
	var subjects []string
	if err := json.Unmarshal(row.Subjects, &subjects); err != nil {
		return storage.LessonObservation{}, fmt.Errorf("observation %s subjects: %w", uuid.UUID(row.ID.Bytes), err)
	}
	return storage.LessonObservation{
		ID:        uuid.UUID(row.ID.Bytes).String(),
		LessonID:  uuid.UUID(row.LessonID.Bytes).String(),
		Author:    row.Author,
		Notes:     row.Notes,
		Subjects:  subjects,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// ensure the interface is satisfied at compile time.
var _ storage.LessonRepository = (*LessonRepository)(nil)
