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
	q *sqlcgen.Queries
}

func NewLessonRepository(pool *pgxpool.Pool) *LessonRepository {
	return &LessonRepository{q: sqlcgen.New(pool)}
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

// Complete sets id's Status to "completed" and CompletedAt to now,
// identity-scoped like Get.
func (r *LessonRepository) Complete(ctx context.Context, identity learner.IdentityID, id string) (storage.Lesson, error) {
	lessonID, err := parseUUID(id)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lesson id: %w", err)
	}
	row, err := r.q.CompleteLesson(ctx, sqlcgen.CompleteLessonParams{
		ID:          lessonID,
		IdentityID:  string(identity),
		CompletedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.Lesson{}, storage.ErrNotFound
		}
		return storage.Lesson{}, err
	}
	return fromLessonRow(row), nil
}

// AddObservation persists o only if o.LessonID actually belongs to
// identity — verified via a join to lessons inside the query itself
// (see db/queries/lessons.sql's InsertLessonObservation): a lesson ID
// that doesn't exist, or exists but belongs to a different identity,
// both come back storage.ErrNotFound via the :execrows zero-rows check,
// mirroring AIRatingRepository.Upsert.
func (r *LessonRepository) AddObservation(ctx context.Context, identity learner.IdentityID, o storage.LessonObservation) error {
	id, err := parseUUID(o.ID)
	if err != nil {
		return fmt.Errorf("observation id: %w", err)
	}
	lessonID, err := parseUUID(o.LessonID)
	if err != nil {
		return fmt.Errorf("lesson id: %w", err)
	}
	subjects := o.Subjects
	if subjects == nil {
		subjects = []string{}
	}
	subjectsJSON, err := json.Marshal(subjects)
	if err != nil {
		return fmt.Errorf("subjects: %w", err)
	}
	rows, err := r.q.InsertLessonObservation(ctx, sqlcgen.InsertLessonObservationParams{
		ID:         id,
		LessonID:   lessonID,
		Author:     o.Author,
		Notes:      o.Notes,
		Subjects:   subjectsJSON,
		CreatedAt:  pgtype.Timestamptz{Time: o.CreatedAt, Valid: true},
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

// Observations returns every observation recorded against lessonID,
// newest first, scoped via the same join AddObservation uses — see
// storage.LessonRepository.Observations' doc comment for the "filters,
// doesn't error" contract this implements.
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
