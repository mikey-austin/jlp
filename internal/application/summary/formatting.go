package summary

import (
	"fmt"

	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// formatStats turns stats into learner-readable lines. Statistics is
// lifetime-cumulative (application/analytics.Service.Statistics
// aggregates across the identity's WHOLE history, not a rolling week —
// PRD §51's richer, week-windowed analytics is a later task), so this
// phrases every line as "so far" rather than claiming "this week",
// which would misrepresent the numbers' actual scope to the model and,
// downstream, to the learner reading the email.
func formatStats(stats storage.Statistics) []string {
	lines := []string{
		fmt.Sprintf("Written %d characters across %d sessions so far.", stats.RunesWritten, stats.SessionCount),
		fmt.Sprintf("Requested feedback %d times, with %d corrections presented.", stats.FeedbackRequests, stats.CorrectionsPresented),
	}
	// AcceptanceRate is only meaningful once at least one correction has
	// been resolved (see analytics.Service.Statistics' own doc comment:
	// it's left 0 otherwise) — omitting the line entirely for a
	// brand-new learner avoids reporting a misleading "0% accepted".
	if stats.CorrectionsAccepted+stats.CorrectionsRejected > 0 {
		lines = append(lines, fmt.Sprintf("Accepted %.0f%% of corrections presented so far.", stats.AcceptanceRate*100))
	}
	return lines
}

// formatPriorities mirrors application/lessons.Service's own
// formatPriorities (in turn mirroring application/feedback.Service.
// recentErrors) exactly, so a learner reading their own summary and a
// tutor reading a lesson guide see the same priority phrased the same
// way.
func formatPriorities(ps []storage.Priority) []string {
	lines := make([]string, 0, len(ps))
	for _, p := range ps {
		lines = append(lines, fmt.Sprintf("%s (%s weakness, score %.1f): %s", p.Subject, p.SubjectType, p.Score, p.Reason))
	}
	return lines
}

// formatExpressions mirrors application/lessons.Service's own
// formatExpressions exactly.
func formatExpressions(items []vocabulary.Item) []string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if item.Reading != "" && item.Reading != item.Expression {
			lines = append(lines, fmt.Sprintf("%s (%s) — %s", item.Expression, item.Reading, item.Meaning))
		} else {
			lines = append(lines, fmt.Sprintf("%s — %s", item.Expression, item.Meaning))
		}
	}
	return lines
}
