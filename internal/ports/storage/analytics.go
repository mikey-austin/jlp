package storage

import (
	"context"
	"time"

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

// VocabFunnel is the three stages of the dashboard's 語彙ファネル section
// (PRD §15/§51): raw sums across an identity's whole vocabulary bank —
// how many expressions were looked up, how many were subsequently
// produced, and how many of those productions were successful. Each
// stage is a superset of the next only in intent, not enforced by the
// query (a word can be "produced" via a route that never counted as a
// "lookup" first, e.g. seeded bank entries).
type VocabFunnel struct{ LookedUp, Produced, ProducedCorrectly int }

// WeeklyCount is one zero-filled week in a SubjectTrend's sparkline.
type WeeklyCount struct {
	WeekStart time.Time
	Count     int
}

// SubjectTrend is one subject's 8-week occurrence sparkline in the
// dashboard's 弱点トレンド section: the last 8 ISO weeks (Monday-aligned
// UTC), oldest first, zero-filled — a week with no occurrences is an
// explicit WeeklyCount{Count: 0} entry, never a gap.
type SubjectTrend struct {
	Subject string
	Weeks   []WeeklyCount
}

// ConfidenceCalibration is one row of the dashboard's 自信の較正 table:
// among exercise_attempts that carry a self-reported Confidence
// (1..5), how many attempts landed at that level and what fraction
// were correct. Only confidence levels that actually occur are
// returned — an identity with no confidence-rated attempts at all gets
// an empty slice, which callers must render as an explicit empty
// state rather than computing anything that could divide by zero.
type ConfidenceCalibration struct {
	Confidence  int
	Attempts    int
	CorrectRate float64
}

// AgentUsage is one row of /learner's エージェント利用 table: per-agent
// (ai.StructuredRequest.Agent — "teacher", "drill", "anki", "lesson",
// "summary") request volume, success rate, and average latency drawn
// from ai_requests. Agent is "" for rows written before that column
// existed (see the 00017 migration) — display code must label that
// bucket, not drop or blank it (see learner.html.tmpl).
type AgentUsage struct {
	Agent        string
	Requests     int
	SuccessRate  float64
	AvgLatencyMS int
}

// SystemStats is /learner's システム section: identity-scoped totals
// across the raw event/audit tables — how much has been logged, not a
// derived quality metric. Scoped to one identity like everything else
// here (PRD single-learner deployment), not a cross-tenant system
// total.
type SystemStats struct {
	LearningEvents int
	EventsByType   map[string]int
	AIRequests     int
}

// AnalyticsRepository computes a learner's aggregate Statistics from
// their documents, sessions, feedback requests, and corrections, plus
// the richer Task 7 views: vocabulary funnel, weakness trends,
// confidence calibration, and agent/system usage.
type AnalyticsRepository interface {
	Statistics(ctx context.Context, identity learner.IdentityID) (Statistics, error)
	VocabFunnel(ctx context.Context, identity learner.IdentityID) (VocabFunnel, error)
	// WeaknessTrends returns an 8-week, zero-filled occurrence sparkline
	// for each of identity's top 3 live weaknesses (learner_observations
	// kind='weakness', most confident first).
	WeaknessTrends(ctx context.Context, identity learner.IdentityID) ([]SubjectTrend, error)
	ConfidenceCalibration(ctx context.Context, identity learner.IdentityID) ([]ConfidenceCalibration, error)
	AgentUsage(ctx context.Context, identity learner.IdentityID) ([]AgentUsage, error)
	SystemStats(ctx context.Context, identity learner.IdentityID) (SystemStats, error)
}
