package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
)

// ObservationRepository persists the learner model: learner_observations,
// UNIQUE on (identity_id, subject_type, subject) — see the 00009
// migration and db/queries/observations.sql for the ON CONFLICT
// replace semantics Upsert relies on.
type ObservationRepository struct{ q *sqlcgen.Queries }

func NewObservationRepository(pool *pgxpool.Pool) *ObservationRepository {
	return &ObservationRepository{q: sqlcgen.New(pool)}
}

func (r *ObservationRepository) Upsert(ctx context.Context, o learnermodel.Observation) error {
	id, err := parseUUID(o.ID)
	if err != nil {
		return fmt.Errorf("observation id: %w", err)
	}
	evidence := o.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	return r.q.UpsertObservation(ctx, sqlcgen.UpsertObservationParams{
		ID:          id,
		IdentityID:  string(o.IdentityID),
		Kind:        string(o.Kind),
		SubjectType: string(o.SubjectType),
		Subject:     o.Subject,
		Confidence:  o.Confidence,
		Evidence:    evidenceJSON,
		FirstSeen:   pgtype.Timestamptz{Time: o.FirstSeen, Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: o.UpdatedAt, Valid: true},
	})
}

func (r *ObservationRepository) List(ctx context.Context, identity learner.IdentityID) ([]learnermodel.Observation, error) {
	rows, err := r.q.ListObservations(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]learnermodel.Observation, 0, len(rows))
	for _, row := range rows {
		o, err := fromObservationRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (r *ObservationRepository) DeleteAll(ctx context.Context, identity learner.IdentityID) error {
	return r.q.DeleteObservations(ctx, string(identity))
}

func fromObservationRow(row sqlcgen.LearnerObservation) (learnermodel.Observation, error) {
	var evidence map[string]any
	if err := json.Unmarshal(row.Evidence, &evidence); err != nil {
		return learnermodel.Observation{}, fmt.Errorf("observation %s evidence: %w", uuid.UUID(row.ID.Bytes), err)
	}
	return learnermodel.Observation{
		ID:          uuid.UUID(row.ID.Bytes).String(),
		IdentityID:  learner.IdentityID(row.IdentityID),
		Kind:        learnermodel.ObservationKind(row.Kind),
		SubjectType: learnermodel.SubjectType(row.SubjectType),
		Subject:     row.Subject,
		Confidence:  row.Confidence,
		Evidence:    evidence,
		FirstSeen:   row.FirstSeen.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}
