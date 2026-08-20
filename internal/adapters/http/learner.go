package httpx

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// learnerPriorityLimit caps the /learner page's priority table — an
// at-a-glance read on what the planner currently thinks matters most,
// not a paginated archive (mirrors grammarConceptCorrectionsLimit's
// role on the /grammar detail page).
const learnerPriorityLimit = 20

// learnerRetrievalLimit caps the /learner page's 復習キュー (review
// queue) table, same role as learnerPriorityLimit above — an
// at-a-glance read on what's scheduled soonest, not a full archive of
// every subject ever reviewed.
const learnerRetrievalLimit = 20

// learnerObservationLimit caps 観察. Unlike the two above it is a cap on
// an unbounded list rather than on a query, so the remainder is counted
// and shown — see learnerPage.
const learnerObservationLimit = 12

// learnerPage handles GET /learner: the identity's current teaching
// priorities (Task 5's heuristic planner output, PRD §16) alongside the
// raw learner-model observations they're derived from — the adapt
// loop's state made visible, not just fed silently into the Teacher
// prompt's RecentErrors. Task 7 (PRD §15/§51) adds エージェント利用
// (AgentUsage — which AI capability is making requests, at what
// success rate/latency) and システム (SystemStats — raw learning-event
// and AI-request totals). Phase 4 Task 7 (PRD §54) adds 復習キュー
// (Retrieval — the spaced-retrieval schedule application/retrieval.
// Scheduler.RecordOutcome maintains: what's due, when, at what
// interval), read straight from storage.RetrievalRepository.List — a
// GET-only listing page, same "raw repository, not the mutating
// Scheduler" split Priorities/Observations already use.
func (s *Server) learnerPage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	priorities, err := s.opts.Priorities.Top(r.Context(), ident.ID, learnerPriorityLimit)
	if err != nil {
		http.Error(w, "could not load priorities", http.StatusInternalServerError)
		return
	}
	observations, err := s.opts.Observations.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load observations", http.StatusInternalServerError)
		return
	}
	retrieval, err := s.opts.Retrieval.List(r.Context(), ident.ID, learnerRetrievalLimit)
	if err != nil {
		http.Error(w, "could not load retrieval queue", http.StatusInternalServerError)
		return
	}
	agentUsage, err := s.opts.Analytics.AgentUsage(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load agent usage", http.StatusInternalServerError)
		return
	}
	// The queue stores a vocabulary ID for an "expression" subject, and a
	// table of UUIDs tells a learner nothing about what they are being
	// asked to review. Resolved here, in ONE query for the whole page.
	// Built as an interface only when the service is actually there: a
	// nil *appvocabulary.Service assigned straight to an interface is a
	// NON-nil interface holding a nil pointer, so withSubjectLabels'
	// own nil check would pass and the call would dereference it.
	var vocab vocabularyByIDs
	if s.opts.Vocabulary != nil {
		vocab = s.opts.Vocabulary
	}
	retrievalRows := withSubjectLabels(r.Context(), vocab, ident.ID, retrieval)
	// 観察 and 優先項目 carry vocabulary ids too, now that a repeatedly
	// failed word becomes a weakness. Same resolution, same reason.
	observationRows := withObservationLabels(r.Context(), vocab, ident.ID, observations)
	priorityRows := withPriorityLabels(r.Context(), vocab, ident.ID, priorities)

	// Observations are unbounded — every subject the model has ever
	// formed a view about — and the panel is a summary, not an archive.
	// The remainder is COUNTED rather than silently dropped, because a
	// list that quietly stops is a list nobody knows is incomplete.
	observationsHidden := 0
	if len(observationRows) > learnerObservationLimit {
		observationsHidden = len(observationRows) - learnerObservationLimit
		observationRows = observationRows[:learnerObservationLimit]
	}

	practice, err := s.opts.Analytics.PracticeStats(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load practice stats", http.StatusInternalServerError)
		return
	}
	system, err := s.opts.Analytics.SystemStats(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load system stats", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "learner", map[string]any{
		"Title":              "学習",
		"Identity":           ident,
		"Priorities":         insightsFromPriorities(priorityRows),
		"Observations":       insightsFromObservations(observationRows),
		"ObservationsHidden": observationsHidden,
		"Retrieval":          insightsFromRetrieval(retrievalRows),
		"Practice":           practiceStatsView(practice),
		"AgentUsage":         agentUsage,
		"System":             system,
	})
}

// practiceStatsView is storage.PracticeStats plus the accuracy the
// template cannot compute (html/template has no division). Accuracy is
// omitted rather than shown as 0% when nothing has been answered:
// "0% correct" and "you have not started" are different statements, and
// only one of them is true on a fresh account.
type practiceStatsViewData struct {
	Answered, Correct, Words, Concepts int
	Accuracy                           int
	HasAnswers                         bool
}

func practiceStatsView(s storage.PracticeStats) practiceStatsViewData {
	v := practiceStatsViewData{
		Answered: s.Answered, Correct: s.Correct,
		Words: s.Words, Concepts: s.Concepts,
		HasAnswers: s.Answered > 0,
	}
	if v.HasAnswers {
		v.Accuracy = s.Correct * 100 / s.Answered
	}
	return v
}

// insightView is one row of 学習's three subject lists: a line that
// expands.
//
// ONE shape for priorities, the review queue and observations, because
// they are the same thing three times — a subject, what kind it is, one
// number that matters, and an explanation that does not belong on the
// line. Three near-identical table markups is how they drifted into
// three different column orders in the first place.
type insightView struct {
	// Label is what to SHOW. A word's subject is a vocabulary id, which
	// tells a learner nothing; resolved before it gets here.
	Label string
	// Kind is the subject type, rendered as a badge.
	Kind string
	// Metric is the one number this list is ordered by, already
	// formatted — a score, a due date, a confidence.
	Metric string
	// Details are the lines behind the disclosure. Usually the planner's
	// scoring rationale, which is diagnostics rather than anything the
	// learner asked for and is the widest thing on the page.
	Details []string
}

// insightsFromPriorities renders the planner's output.
func insightsFromPriorities(rows []priorityRowView) []insightView {
	out := make([]insightView, 0, len(rows))
	for _, r := range rows {
		out = append(out, insightView{
			Label:   r.Label,
			Kind:    r.SubjectType,
			Metric:  strconv.FormatFloat(r.Score, 'f', 1, 64),
			Details: []string{r.Reason},
		})
	}
	return out
}

// insightsFromRetrieval renders the review queue. The metric is the due
// date, because "when" is the only question a queue answers.
func insightsFromRetrieval(rows []retrievalItemView) []insightView {
	out := make([]insightView, 0, len(rows))
	for _, r := range rows {
		out = append(out, insightView{
			Label:  r.Label,
			Kind:   r.SubjectType,
			Metric: r.DueAt.Format("01-02"),
			Details: []string{
				fmt.Sprintf("期日 %s ・ 間隔 %d日", r.DueAt.Format("2006-01-02"), int(r.Interval.Hours()/24)),
				fmt.Sprintf("正解 %d回 ・ 不正解 %d回", r.Successes, r.Failures),
			},
		})
	}
	return out
}

// insightsFromObservations renders the learner model's own view.
func insightsFromObservations(rows []observationRowView) []insightView {
	out := make([]insightView, 0, len(rows))
	for _, r := range rows {
		details := []string{
			fmt.Sprintf("%s ・ 信頼度 %.0f%%", r.Kind, r.Confidence*100),
			"最終更新 " + r.UpdatedAt.Format("2006-01-02"),
		}
		if count, ok := r.Evidence["count"]; ok {
			details = append(details, fmt.Sprintf("直近30日に%v回", count))
		}
		out = append(out, insightView{
			Label:   r.Label,
			Kind:    string(r.SubjectType),
			Metric:  fmt.Sprintf("%.0f%%", r.Confidence*100),
			Details: details,
		})
	}
	return out
}

// retrievalItemView / observationRowView / priorityRowView are their
// repository rows plus a human label for the subject.
//
// The queue stores a vocabulary ID for a word, and a table of UUIDs
// tells a learner nothing about what they are being asked to review.
type retrievalItemView struct {
	storage.RetrievalItem
	Label string
}

type observationRowView struct {
	learnermodel.Observation
	Label string
}

type priorityRowView struct {
	storage.Priority
	Label string
}

// withSubjectLabels resolves every "expression" subject to the word it
// names, in one query.
//
// A failure here is deliberately not fatal: this is a label, and a
// learner is better served by a queue showing raw ids than by a 500 on
// the page that shows their whole learning state.
func withSubjectLabels(ctx context.Context, vocab vocabularyByIDs, identity learner.IdentityID, items []storage.RetrievalItem) []retrievalItemView {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.SubjectType == "expression" {
			ids = append(ids, it.Subject)
		}
	}
	labels := resolveWordLabels(ctx, vocab, identity, ids)

	out := make([]retrievalItemView, 0, len(items))
	for _, it := range items {
		out = append(out, retrievalItemView{RetrievalItem: it, Label: labelFor(labels, it.Subject)})
	}
	return out
}

func withObservationLabels(ctx context.Context, vocab vocabularyByIDs, identity learner.IdentityID, items []learnermodel.Observation) []observationRowView {
	ids := make([]string, 0, len(items))
	for _, o := range items {
		if o.SubjectType == learnermodel.SubjectWord {
			ids = append(ids, o.Subject)
		}
	}
	labels := resolveWordLabels(ctx, vocab, identity, ids)

	out := make([]observationRowView, 0, len(items))
	for _, o := range items {
		out = append(out, observationRowView{Observation: o, Label: labelFor(labels, o.Subject)})
	}
	return out
}

func withPriorityLabels(ctx context.Context, vocab vocabularyByIDs, identity learner.IdentityID, items []storage.Priority) []priorityRowView {
	ids := make([]string, 0, len(items))
	for _, p := range items {
		if p.SubjectType == string(learnermodel.SubjectWord) {
			ids = append(ids, p.Subject)
		}
	}
	labels := resolveWordLabels(ctx, vocab, identity, ids)

	out := make([]priorityRowView, 0, len(items))
	for _, p := range items {
		out = append(out, priorityRowView{Priority: p, Label: labelFor(labels, p.Subject)})
	}
	return out
}

// resolveWordLabels maps vocabulary ids to their expressions, in one
// query, tolerating both a missing service and a failed lookup.
func resolveWordLabels(ctx context.Context, vocab vocabularyByIDs, identity learner.IdentityID, ids []string) map[string]string {
	if vocab == nil || len(ids) == 0 {
		return nil
	}
	resolved, err := vocab.GetByIDs(ctx, identity, ids)
	if err != nil {
		return nil
	}
	labels := make(map[string]string, len(resolved))
	for _, item := range resolved {
		labels[item.ID] = item.Expression
	}
	return labels
}

// labelFor prefers the resolved label and falls back to the raw subject:
// an unfriendly cell beats a blank one.
func labelFor(labels map[string]string, subject string) string {
	if l, ok := labels[subject]; ok && l != "" {
		return l
	}
	return subject
}

// vocabularyByIDs is the one method label resolution needs — narrow so
// the learner page does not depend on the whole vocabulary service, and
// so a test can supply it in one line.
type vocabularyByIDs interface {
	GetByIDs(ctx context.Context, identity learner.IdentityID, ids []string) ([]vocabulary.Item, error)
}
