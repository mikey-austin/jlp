package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// LearningEventRepository stores immutable learning_events rows: Append
// only ever inserts, never updates.
type LearningEventRepository struct{ q *sqlcgen.Queries }

func NewLearningEventRepository(pool *pgxpool.Pool) *LearningEventRepository {
	return &LearningEventRepository{q: sqlcgen.New(pool)}
}

func (r *LearningEventRepository) Append(ctx context.Context, ev event.LearningEvent) error {
	id, err := parseUUID(ev.ID)
	if err != nil {
		return fmt.Errorf("event id: %w", err)
	}
	sid, err := toNullableUUID(ev.SessionID)
	if err != nil {
		return fmt.Errorf("session id: %w", err)
	}
	evidence := ev.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}

	return r.q.AppendLearningEvent(ctx, sqlcgen.AppendLearningEventParams{
		ID:         id,
		IdentityID: string(ev.IdentityID),
		SessionID:  sid,
		Type:       string(ev.Type),
		Subject:    ev.Subject,
		Evidence:   evidenceJSON,
		OccurredAt: pgtype.Timestamptz{Time: ev.OccurredAt, Valid: true},
	})
}

// ListRecent returns up to limit events for identity, newest first. If sid
// is non-nil, results are further scoped to that session.
func (r *LearningEventRepository) ListRecent(ctx context.Context, identity learner.IdentityID, sid *session.ID, limit int) ([]event.LearningEvent, error) {
	pgSID, err := toNullableUUID(sid)
	if err != nil {
		return nil, fmt.Errorf("session id: %w", err)
	}
	rows, err := r.q.ListRecentLearningEvents(ctx, sqlcgen.ListRecentLearningEventsParams{
		IdentityID: string(identity),
		Column2:    pgSID,
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]event.LearningEvent, 0, len(rows))
	for _, row := range rows {
		ev, err := fromLearningEventRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// toNullableUUID converts an optional session.ID to the nullable pgtype
// sqlc generates for the learning_events.session_id column: nil maps to
// an invalid (SQL NULL) pgtype.UUID.
func toNullableUUID(sid *session.ID) (pgtype.UUID, error) {
	if sid == nil {
		return pgtype.UUID{}, nil
	}
	return parseUUID(string(*sid))
}

func fromLearningEventRow(row sqlcgen.LearningEvent) (event.LearningEvent, error) {
	var evidence map[string]any
	if err := json.Unmarshal(row.Evidence, &evidence); err != nil {
		return event.LearningEvent{}, fmt.Errorf("event %s evidence: %w", uuid.UUID(row.ID.Bytes), err)
	}
	var sid *session.ID
	if row.SessionID.Valid {
		s := session.ID(uuid.UUID(row.SessionID.Bytes).String())
		sid = &s
	}
	return event.LearningEvent{
		ID:         uuid.UUID(row.ID.Bytes).String(),
		IdentityID: learner.IdentityID(row.IdentityID),
		SessionID:  sid,
		Type:       event.Type(row.Type),
		Subject:    row.Subject,
		Evidence:   evidence,
		OccurredAt: row.OccurredAt.Time,
	}, nil
}
