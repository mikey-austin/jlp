package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// AgentRunRepository persists agent-run traces (agent_runs + tool_calls,
// see the 00018 migration). tool_calls carries no identity_id of its
// own — RecordToolCall and the tool-call reads below reach it only
// via a join through agent_runs, mirroring FeedbackRepository's
// corrections-via-feedback_requests join.
type AgentRunRepository struct{ q *sqlcgen.Queries }

func NewAgentRunRepository(pool *pgxpool.Pool) *AgentRunRepository {
	return &AgentRunRepository{q: sqlcgen.New(pool)}
}

func (r *AgentRunRepository) Start(ctx context.Context, run storage.AgentRun) error {
	id, err := parseUUID(run.ID)
	if err != nil {
		return fmt.Errorf("agent run id: %w", err)
	}
	sid, err := toNullableUUID(run.SessionID)
	if err != nil {
		return fmt.Errorf("session id: %w", err)
	}
	return r.q.InsertAgentRun(ctx, sqlcgen.InsertAgentRunParams{
		ID:            id,
		IdentityID:    string(run.IdentityID),
		SessionID:     sid,
		Agent:         run.Agent,
		PromptName:    run.PromptName,
		PromptVersion: run.PromptVersion,
		Status:        run.Status,
		Turns:         int32(run.Turns),
		StartedAt:     pgtype.Timestamptz{Time: run.StartedAt, Valid: true},
	})
}

func (r *AgentRunRepository) Finish(ctx context.Context, identity learner.IdentityID, runID, status, errMsg string, turns int, endedAt time.Time) error {
	id, err := parseUUID(runID)
	if err != nil {
		return fmt.Errorf("agent run id: %w", err)
	}
	n, err := r.q.FinishAgentRun(ctx, sqlcgen.FinishAgentRunParams{
		ID:         id,
		IdentityID: string(identity),
		Status:     status,
		Error:      errMsg,
		Turns:      int32(turns),
		EndedAt:    pgtype.Timestamptz{Time: endedAt, Valid: true},
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *AgentRunRepository) RecordToolCall(ctx context.Context, identity learner.IdentityID, c storage.ToolCall) error {
	id, err := parseUUID(c.ID)
	if err != nil {
		return fmt.Errorf("tool call id: %w", err)
	}
	runID, err := parseUUID(c.AgentRunID)
	if err != nil {
		return fmt.Errorf("agent run id: %w", err)
	}
	n, err := r.q.InsertToolCall(ctx, sqlcgen.InsertToolCallParams{
		ID:         id,
		ToolName:   c.ToolName,
		Arguments:  c.Arguments,
		Result:     c.Result,
		IsError:    c.IsError,
		DurationMs: int32(c.DurationMS),
		CreatedAt:  pgtype.Timestamptz{Time: c.CreatedAt, Valid: true},
		ID_2:       runID,
		IdentityID: string(identity),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func (r *AgentRunRepository) List(ctx context.Context, identity learner.IdentityID, limit int) ([]storage.AgentRun, error) {
	rows, err := r.q.ListAgentRuns(ctx, sqlcgen.ListAgentRunsParams{
		IdentityID: string(identity),
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]storage.AgentRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromAgentRunRow(row))
	}
	return out, nil
}

func (r *AgentRunRepository) Get(ctx context.Context, identity learner.IdentityID, runID string) (storage.AgentRun, []storage.ToolCall, error) {
	id, err := parseUUID(runID)
	if err != nil {
		return storage.AgentRun{}, nil, fmt.Errorf("agent run id: %w", err)
	}
	row, err := r.q.GetAgentRun(ctx, sqlcgen.GetAgentRunParams{ID: id, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.AgentRun{}, nil, storage.ErrNotFound
		}
		return storage.AgentRun{}, nil, err
	}

	calls, err := r.q.ListToolCallsForRun(ctx, id)
	if err != nil {
		return storage.AgentRun{}, nil, err
	}
	out := make([]storage.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, fromToolCallRow(c))
	}
	return fromAgentRunRow(row), out, nil
}

func fromAgentRunRow(row sqlcgen.AgentRun) storage.AgentRun {
	run := storage.AgentRun{
		ID:            uuid.UUID(row.ID.Bytes).String(),
		IdentityID:    learner.IdentityID(row.IdentityID),
		Agent:         row.Agent,
		PromptName:    row.PromptName,
		PromptVersion: row.PromptVersion,
		Status:        row.Status,
		Turns:         int(row.Turns),
		StartedAt:     row.StartedAt.Time,
		Error:         row.Error,
	}
	if row.SessionID.Valid {
		sid := session.ID(uuid.UUID(row.SessionID.Bytes).String())
		run.SessionID = &sid
	}
	if row.EndedAt.Valid {
		run.EndedAt = row.EndedAt.Time
	}
	return run
}

func fromToolCallRow(row sqlcgen.ToolCall) storage.ToolCall {
	return storage.ToolCall{
		ID:         uuid.UUID(row.ID.Bytes).String(),
		AgentRunID: uuid.UUID(row.AgentRunID.Bytes).String(),
		ToolName:   row.ToolName,
		Arguments:  row.Arguments,
		Result:     row.Result,
		IsError:    row.IsError,
		DurationMS: int(row.DurationMs),
		CreatedAt:  row.CreatedAt.Time,
	}
}
