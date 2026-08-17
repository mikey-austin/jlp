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
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ConversationRepository persists the conversation tutor's transcript
// (Phase 4 Task 6, PRD §17.4): the conversations and conversation_turns
// tables (00021 migration) — see storage.ConversationRepository's doc
// comment for the full identity-scoping contract.
type ConversationRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewConversationRepository(pool *pgxpool.Pool) *ConversationRepository {
	return &ConversationRepository{pool: pool, q: sqlcgen.New(pool)}
}

// GetOrCreateForSession returns the session's single conversation,
// creating one the first time it's opened — the same
// read-then-insert-on-miss shape DocumentRepository.GetOrCreateForSession
// uses for the writing pane's one document per session.
func (r *ConversationRepository) GetOrCreateForSession(ctx context.Context, identity learner.IdentityID, sid session.ID) (string, bool, error) {
	pgSID, err := parseUUID(string(sid))
	if err != nil {
		return "", false, fmt.Errorf("session id: %w", err)
	}

	row, err := r.q.GetConversationBySession(ctx, sqlcgen.GetConversationBySessionParams{SessionID: pgSID, IdentityID: string(identity)})
	if err == nil {
		return uuid.UUID(row.ID.Bytes).String(), false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}

	id, err := uuid.NewRandom()
	if err != nil {
		return "", false, err
	}
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	row, err = r.q.InsertConversation(ctx, sqlcgen.InsertConversationParams{
		ID:         pgtype.UUID{Bytes: id, Valid: true},
		SessionID:  pgSID,
		IdentityID: string(identity),
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		// InsertConversation is guarded on the session being live (see
		// db/queries/conversations.sql), so zero rows means "that
		// session is deleted, or was never this identity's". Mapped to
		// storage.ErrNotFound for the same reason DocumentRepository.
		// GetOrCreateForSession maps it: callers branch on ErrNotFound
		// to answer 404 rather than 500.
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, storage.ErrNotFound
		}
		return "", false, err
	}
	return uuid.UUID(row.ID.Bytes).String(), true, nil
}

// InsertTurn appends turn to conversationID, identity-scoped via a join
// to conversations (db/queries/conversations.sql's InsertConversationTurn):
// zero rows affected — conversationID doesn't exist, or belongs to a
// different identity — maps to storage.ErrNotFound.
func (r *ConversationRepository) InsertTurn(ctx context.Context, identity learner.IdentityID, conversationID string, turn storage.ConversationTurn) error {
	tid, err := parseUUID(turn.ID)
	if err != nil {
		return fmt.Errorf("turn id: %w", err)
	}
	cid, err := parseUUID(conversationID)
	if err != nil {
		return fmt.Errorf("conversation id: %w", err)
	}
	aiRequestID, err := toOptionalUUID(turn.AIRequestID)
	if err != nil {
		return fmt.Errorf("ai request id: %w", err)
	}
	corrections := turn.Corrections
	if corrections == nil {
		corrections = []correction.Correction{}
	}
	correctionsJSON, err := json.Marshal(corrections)
	if err != nil {
		return fmt.Errorf("corrections: %w", err)
	}

	rows, err := r.q.InsertConversationTurn(ctx, sqlcgen.InsertConversationTurnParams{
		ID:             tid,
		ConversationID: cid,
		Position:       int32(turn.Position),
		LearnerText:    turn.LearnerText,
		Reply:          turn.Reply,
		ReplyEn:        turn.ReplyEN,
		Followup:       turn.Followup,
		Corrections:    correctionsJSON,
		AiRequestID:    aiRequestID,
		CreatedAt:      pgtype.Timestamptz{Time: turn.CreatedAt, Valid: true},
		IdentityID:     string(identity),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// ListTurns returns every turn in conversationID, oldest first,
// identity-scoped via the same join InsertTurn uses.
func (r *ConversationRepository) ListTurns(ctx context.Context, identity learner.IdentityID, conversationID string) ([]storage.ConversationTurn, error) {
	cid, err := parseUUID(conversationID)
	if err != nil {
		return nil, fmt.Errorf("conversation id: %w", err)
	}
	rows, err := r.q.ListConversationTurns(ctx, sqlcgen.ListConversationTurnsParams{ConversationID: cid, IdentityID: string(identity)})
	if err != nil {
		return nil, err
	}
	out := make([]storage.ConversationTurn, 0, len(rows))
	for _, row := range rows {
		t, err := fromConversationTurnRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// fromConversationTurnRow builds a storage.ConversationTurn from a
// ListConversationTurns row, including SessionID (populated via that
// query's join back to conversations — see db/queries/conversations.sql's
// own doc comment on why conversation_turns itself has no session_id
// column to select directly).
func fromConversationTurnRow(row sqlcgen.ListConversationTurnsRow) (storage.ConversationTurn, error) {
	var corrections []correction.Correction
	if err := json.Unmarshal(row.Corrections, &corrections); err != nil {
		return storage.ConversationTurn{}, fmt.Errorf("turn %s corrections: %w", uuid.UUID(row.ID.Bytes), err)
	}
	var aiRequestID string
	if row.AiRequestID.Valid {
		aiRequestID = uuid.UUID(row.AiRequestID.Bytes).String()
	}
	return storage.ConversationTurn{
		ID:             uuid.UUID(row.ID.Bytes).String(),
		ConversationID: uuid.UUID(row.ConversationID.Bytes).String(),
		SessionID:      session.ID(uuid.UUID(row.SessionID.Bytes).String()),
		Position:       int(row.Position),
		LearnerText:    row.LearnerText,
		Reply:          row.Reply,
		ReplyEN:        row.ReplyEn,
		Followup:       row.Followup,
		Corrections:    corrections,
		AIRequestID:    aiRequestID,
		CreatedAt:      row.CreatedAt.Time,
	}, nil
}

// ensure the interface is satisfied at compile time.
var _ storage.ConversationRepository = (*ConversationRepository)(nil)
