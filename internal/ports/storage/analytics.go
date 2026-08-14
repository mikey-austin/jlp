package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// ErrorTypeCount is one entry of Statistics.TopErrorTypes: how many
// times a correction.Type was presented to the learner, most frequent
// first.
type ErrorTypeCount struct {
	Type  string
	Count int
}

// Statistics is a learner's aggregate stats across their whole history
// — writing volume, feedback engagement, and correction outcomes.
// AnalyticsRepository returns this with AcceptanceRate and
// CorrectionsPer1000 left zero; application/analytics.Service computes
// those two from the raw counts below, which keeps the derivation
// unit-testable without a database (see that package for why the split
// exists).
type Statistics struct {
	RunesWritten         int // sum of char_length(content) over the identity's documents
	SessionCount         int
	FeedbackRequests     int
	CorrectionsPresented int
	CorrectionsAccepted  int
	CorrectionsRejected  int
	AcceptanceRate       float64          // accepted / (accepted+rejected), 0 when no resolutions
	CorrectionsPer1000   float64          // presented per 1000 runes written, 0 when no runes
	TopErrorTypes        []ErrorTypeCount // top 5 by presented corrections, most frequent first
}

// AnalyticsRepository computes a learner's aggregate Statistics from
// their documents, sessions, feedback requests, and corrections.
type AnalyticsRepository interface {
	Statistics(ctx context.Context, identity learner.IdentityID) (Statistics, error)
}
