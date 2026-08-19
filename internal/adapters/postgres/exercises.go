package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ExerciseRepository persists generated drill exercises and the
// learner's attempts at them: exercises, exercise_attempts — see the
// 00013 migration and storage.ExerciseRepository's doc comment.
type ExerciseRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewExerciseRepository(pool *pgxpool.Pool) *ExerciseRepository {
	return &ExerciseRepository{pool: pool, q: sqlcgen.New(pool)}
}

// exercisePayload is exercises.payload's full JSON shape: the whole
// Exercise domain struct, not just the fields not already broken out
// into their own columns — per the task brief ("payload = full Exercise
// JSON"). identity_id/concept_slug/type/created_at are ALSO stored as
// separate columns (for future indexing/reporting queries), but payload
// alone is what Get reads back — see GetExercise's query, which selects
// only that column.
type exercisePayload struct {
	ID             string    `json:"id"`
	IdentityID     string    `json:"identity_id"`
	SessionID      *string   `json:"session_id,omitempty"`
	ConceptSlug    string    `json:"concept_slug"`
	SubjectType    string    `json:"subject_type,omitempty"`
	SubjectRef     string    `json:"subject_ref,omitempty"`
	Type           string    `json:"type"`
	InstructionsJA string    `json:"instructions_ja"`
	InstructionsEN string    `json:"instructions_en"`
	Prompt         string    `json:"prompt"`
	Choices        []string  `json:"choices,omitempty"`
	Answer         string    `json:"answer"`
	Acceptable     []string  `json:"acceptable,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Create persists ex: the full record as payload jsonb, plus
// id/identity_id/session_id/concept_slug/type/created_at broken out as
// their own columns.
func (r *ExerciseRepository) Create(ctx context.Context, ex exercise.Exercise) error {
	id, err := parseUUID(ex.ID)
	if err != nil {
		return fmt.Errorf("exercise: id: %w", err)
	}

	var sessionIDPg pgtype.UUID
	var sessionStr *string
	if ex.SessionID != nil {
		sid, err := parseUUID(string(*ex.SessionID))
		if err != nil {
			return fmt.Errorf("exercise: session id: %w", err)
		}
		sessionIDPg = sid
		s := string(*ex.SessionID)
		sessionStr = &s
	}

	payload, err := json.Marshal(exercisePayload{
		ID:             ex.ID,
		IdentityID:     string(ex.IdentityID),
		SessionID:      sessionStr,
		ConceptSlug:    ex.ConceptSlug,
		SubjectType:    ex.SubjectType,
		SubjectRef:     ex.SubjectRef,
		Type:           ex.Type,
		InstructionsJA: ex.InstructionsJA,
		InstructionsEN: ex.InstructionsEN,
		Prompt:         ex.Prompt,
		Choices:        ex.Choices,
		Answer:         ex.Answer,
		Acceptable:     ex.Acceptable,
		CreatedAt:      ex.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("exercise: marshal payload: %w", err)
	}

	return r.q.InsertExercise(ctx, sqlcgen.InsertExerciseParams{
		ID:          id,
		IdentityID:  string(ex.IdentityID),
		SessionID:   sessionIDPg,
		ConceptSlug: ex.ConceptSlug,
		Type:        ex.Type,
		Payload:     payload,
		CreatedAt:   pgtype.Timestamptz{Time: ex.CreatedAt, Valid: true},
	})
}

// Get is identity-scoped via GetExercise's WHERE clause: an exercise ID
// that exists but belongs to a different identity misses with
// storage.ErrNotFound, same as every other identity-scoped repository.
func (r *ExerciseRepository) Get(ctx context.Context, identity learner.IdentityID, id string) (exercise.Exercise, error) {
	exID, err := parseUUID(id)
	if err != nil {
		return exercise.Exercise{}, fmt.Errorf("exercise: id: %w", err)
	}

	raw, err := r.q.GetExercise(ctx, sqlcgen.GetExerciseParams{ID: exID, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return exercise.Exercise{}, storage.ErrNotFound
		}
		return exercise.Exercise{}, err
	}

	var payload exercisePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return exercise.Exercise{}, fmt.Errorf("exercise: unmarshal payload: %w", err)
	}

	ex := exercise.Exercise{
		ID:             payload.ID,
		IdentityID:     learner.IdentityID(payload.IdentityID),
		ConceptSlug:    payload.ConceptSlug,
		SubjectType:    payload.SubjectType,
		SubjectRef:     payload.SubjectRef,
		Type:           payload.Type,
		InstructionsJA: payload.InstructionsJA,
		InstructionsEN: payload.InstructionsEN,
		Prompt:         payload.Prompt,
		Choices:        payload.Choices,
		Answer:         payload.Answer,
		Acceptable:     payload.Acceptable,
		CreatedAt:      payload.CreatedAt,
	}
	// Rows written before exercises carried a subject: every one of them
	// was a concept drill, because that was the only kind there was.
	// Normalised here rather than left empty so nothing downstream has to
	// know the field arrived late.
	if ex.SubjectType == "" {
		ex.SubjectType = exercise.SubjectConcept
		ex.SubjectRef = payload.ConceptSlug
	}
	if payload.SessionID != nil {
		sid := session.ID(*payload.SessionID)
		ex.SessionID = &sid
	}
	return ex, nil
}

// RecordAttempt persists at, which the caller
// (application/practice.Service.Answer) has already fully populated.
func (r *ExerciseRepository) RecordAttempt(ctx context.Context, at storage.ExerciseAttempt) error {
	id, err := parseUUID(at.ID)
	if err != nil {
		return fmt.Errorf("exercise attempt: id: %w", err)
	}
	exID, err := parseUUID(at.ExerciseID)
	if err != nil {
		return fmt.Errorf("exercise attempt: exercise id: %w", err)
	}

	feedback, err := json.Marshal(map[string]string{"ja": at.FeedbackJA, "en": at.FeedbackEN})
	if err != nil {
		return fmt.Errorf("exercise attempt: marshal feedback: %w", err)
	}

	var confidence pgtype.Int4
	if at.Confidence != nil {
		confidence = pgtype.Int4{Int32: int32(*at.Confidence), Valid: true}
	}

	return r.q.InsertExerciseAttempt(ctx, sqlcgen.InsertExerciseAttemptParams{
		ID:         id,
		ExerciseID: exID,
		Response:   at.Response,
		Correct:    pgtype.Bool{Bool: at.Correct, Valid: true},
		Score:      pgtype.Int4{Int32: int32(at.Score), Valid: true},
		Feedback:   feedback,
		Confidence: confidence,
		CreatedAt:  pgtype.Timestamptz{Time: at.CreatedAt, Valid: true},
	})
}

// ensure the interface is satisfied at compile time.
var _ storage.ExerciseRepository = (*ExerciseRepository)(nil)
