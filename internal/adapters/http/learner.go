package httpx

import (
	"context"
	"net/http"

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
		"Title":        "学習",
		"Identity":     ident,
		"Priorities":   priorityRows,
		"Observations": observationRows,
		"Retrieval":    retrievalRows,
		"Practice":     practiceStatsView(practice),
		"AgentUsage":   agentUsage,
		"System":       system,
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

// retrievalItemView is a storage.RetrievalItem with a human label for
// its subject. Everything else is passed through, so the template keeps
// reading the same fields it always did.
type retrievalItemView struct {
	storage.RetrievalItem
	// Label is what to SHOW: the expression for a word, the slug for a
	// concept. Never empty — an unresolvable subject falls back to the
	// raw value rather than rendering a blank row.
	Label string
}

// withSubjectLabels resolves every "expression" subject to the word it
// names, in one query.
//
// A failure here is deliberately not fatal: this is a label, and a
// learner is better served by a queue showing raw ids than by a 500 on
// the page that shows their whole learning state.
func withSubjectLabels(ctx context.Context, vocab vocabularyByIDs, identity learner.IdentityID, items []storage.RetrievalItem) []retrievalItemView {
	out := make([]retrievalItemView, 0, len(items))
	var ids []string
	for _, it := range items {
		if it.SubjectType == "expression" {
			ids = append(ids, it.Subject)
		}
	}

	labels := map[string]string{}
	if vocab != nil && len(ids) > 0 {
		if resolved, err := vocab.GetByIDs(ctx, identity, ids); err == nil {
			for _, item := range resolved {
				labels[item.ID] = item.Expression
			}
		}
	}

	for _, it := range items {
		label := it.Subject
		if l, ok := labels[it.Subject]; ok && l != "" {
			label = l
		}
		out = append(out, retrievalItemView{RetrievalItem: it, Label: label})
	}
	return out
}

// vocabularyByIDs is the one method withSubjectLabels needs — narrow so
// the learner page does not depend on the whole vocabulary service, and
// so a test can supply it in one line.
type vocabularyByIDs interface {
	GetByIDs(ctx context.Context, identity learner.IdentityID, ids []string) ([]vocabulary.Item, error)
}

// observationRowView / priorityRowView are the same "row plus a human
// label" shape as retrievalItemView, for the two other tables that can
// now carry a vocabulary id as a subject.
type observationRowView struct {
	learnermodel.Observation
	Label string
}

type priorityRowView struct {
	storage.Priority
	Label string
}

func withObservationLabels(ctx context.Context, vocab vocabularyByIDs, identity learner.IdentityID, items []learnermodel.Observation) []observationRowView {
	subjects := make([]string, 0, len(items))
	for _, o := range items {
		if o.SubjectType == learnermodel.SubjectWord {
			subjects = append(subjects, o.Subject)
		}
	}
	labels := resolveWordLabels(ctx, vocab, identity, subjects)

	out := make([]observationRowView, 0, len(items))
	for _, o := range items {
		out = append(out, observationRowView{Observation: o, Label: labelFor(labels, o.Subject)})
	}
	return out
}

func withPriorityLabels(ctx context.Context, vocab vocabularyByIDs, identity learner.IdentityID, items []storage.Priority) []priorityRowView {
	subjects := make([]string, 0, len(items))
	for _, p := range items {
		if p.SubjectType == string(learnermodel.SubjectWord) {
			subjects = append(subjects, p.Subject)
		}
	}
	labels := resolveWordLabels(ctx, vocab, identity, subjects)

	out := make([]priorityRowView, 0, len(items))
	for _, p := range items {
		out = append(out, priorityRowView{Priority: p, Label: labelFor(labels, p.Subject)})
	}
	return out
}

// resolveWordLabels maps vocabulary ids to their expressions, in one
// query, tolerating both a missing service and a failed lookup — see
// withSubjectLabels for why a label is never worth failing a page over.
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
