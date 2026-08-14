package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// AIRequestRecord is one row of the ai_requests audit log: every call
// made through an ai.StructuredGenerator wrapped by
// observability.NewAIObserver, success or failure, latency and cost
// included.
type AIRequestRecord struct {
	ID            string
	Capability    string
	Provider      string
	Model         string
	PromptName    string
	PromptVersion string
	IdentityID    learner.IdentityID
	SessionID     *session.ID
	LatencyMS     int
	InputTokens   int
	OutputTokens  int
	CostUSD       float64
	Success       bool
	Error         string
	CreatedAt     time.Time
}

// AIRequestRepository persists the audit log. Insert is called for
// every generator invocation, success or failure — observability must
// never lose a record.
type AIRequestRepository interface {
	Insert(ctx context.Context, rec AIRequestRecord) error
	// List returns up to limit records for identity, newest first.
	List(ctx context.Context, identity learner.IdentityID, limit int) ([]AIRequestRecord, error)
}
