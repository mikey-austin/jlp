package httpx

import (
	"net/http"
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
	system, err := s.opts.Analytics.SystemStats(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load system stats", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "learner", map[string]any{
		"Title":        "学習",
		"Identity":     ident,
		"Priorities":   priorities,
		"Observations": observations,
		"Retrieval":    retrieval,
		"AgentUsage":   agentUsage,
		"System":       system,
	})
}
