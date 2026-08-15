package postgres

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"

	"github.com/jackc/pgx/v5/pgtype"
)

// AIRequestRepository is the audit log of every ai.StructuredGenerator
// call, written by the observability decorator: append-only, like
// LearningEventRepository.
type AIRequestRepository struct{ q *sqlcgen.Queries }

func NewAIRequestRepository(pool *pgxpool.Pool) *AIRequestRepository {
	return &AIRequestRepository{q: sqlcgen.New(pool)}
}

func (r *AIRequestRepository) Insert(ctx context.Context, rec storage.AIRequestRecord) error {
	id, err := parseUUID(rec.ID)
	if err != nil {
		return fmt.Errorf("ai request id: %w", err)
	}
	sid, err := toNullableUUID(rec.SessionID)
	if err != nil {
		return fmt.Errorf("session id: %w", err)
	}
	cost, err := floatToNumeric(rec.CostUSD)
	if err != nil {
		return fmt.Errorf("cost usd: %w", err)
	}

	return r.q.InsertAIRequest(ctx, sqlcgen.InsertAIRequestParams{
		ID:            id,
		IdentityID:    string(rec.IdentityID),
		SessionID:     sid,
		Capability:    rec.Capability,
		Provider:      rec.Provider,
		Model:         rec.Model,
		PromptName:    rec.PromptName,
		PromptVersion: rec.PromptVersion,
		LatencyMs:     int32(rec.LatencyMS),
		InputTokens:   int32(rec.InputTokens),
		OutputTokens:  int32(rec.OutputTokens),
		CostUsd:       cost,
		Success:       rec.Success,
		Error:         rec.Error,
		CreatedAt:     pgtype.Timestamptz{Time: rec.CreatedAt, Valid: true},
		Agent:         rec.Agent,
	})
}

// List returns up to limit records for identity, newest first.
func (r *AIRequestRepository) List(ctx context.Context, identity learner.IdentityID, limit int) ([]storage.AIRequestRecord, error) {
	rows, err := r.q.ListAIRequests(ctx, sqlcgen.ListAIRequestsParams{
		IdentityID: string(identity),
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]storage.AIRequestRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := fromAIRequestRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func fromAIRequestRow(row sqlcgen.AiRequest) (storage.AIRequestRecord, error) {
	cost, err := numericToFloat(row.CostUsd)
	if err != nil {
		return storage.AIRequestRecord{}, fmt.Errorf("cost usd: %w", err)
	}
	var sid *session.ID
	if row.SessionID.Valid {
		s := session.ID(uuid.UUID(row.SessionID.Bytes).String())
		sid = &s
	}
	return storage.AIRequestRecord{
		ID:            uuid.UUID(row.ID.Bytes).String(),
		Capability:    row.Capability,
		Provider:      row.Provider,
		Model:         row.Model,
		PromptName:    row.PromptName,
		PromptVersion: row.PromptVersion,
		IdentityID:    learner.IdentityID(row.IdentityID),
		SessionID:     sid,
		LatencyMS:     int(row.LatencyMs),
		InputTokens:   int(row.InputTokens),
		OutputTokens:  int(row.OutputTokens),
		CostUSD:       cost,
		Success:       row.Success,
		Error:         row.Error,
		CreatedAt:     row.CreatedAt.Time,
		Agent:         row.Agent,
	}, nil
}

// floatToNumeric and numericToFloat convert the cost_usd numeric(10,6)
// column to/from the float64 storage.AIRequestRecord.CostUSD carries.
// pgtype.Numeric is an arbitrary-precision decimal (big.Int mantissa +
// exponent); dollar costs computed from token counts and per-MTok
// rates never need that precision, so we round-trip through a
// formatted decimal string at the column's own scale (6 places) rather
// than build a big.Int by hand.
func floatToNumeric(f float64) (pgtype.Numeric, error) {
	var n pgtype.Numeric
	if err := n.Scan(strconv.FormatFloat(f, 'f', 6, 64)); err != nil {
		return pgtype.Numeric{}, err
	}
	return n, nil
}

func numericToFloat(n pgtype.Numeric) (float64, error) {
	f8, err := n.Float64Value()
	if err != nil {
		return 0, err
	}
	return f8.Float64, nil
}
