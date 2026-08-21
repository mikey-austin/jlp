package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// grammarConceptCorrectionsLimit caps the detail page's recent-corrections
// list — an at-a-glance sample of how this concept has come up for the
// identity, not a paginated archive (mirrors aiRequestsPageLimit's role
// on the /ai page).
const grammarConceptCorrectionsLimit = 10

// jlptChip formats a grammar.Concept/storage.ConceptStat JLPTLevel (1..5,
// 1 hardest) as the learner-facing "N1".."N5" label.
func jlptChip(level int) string {
	return fmt.Sprintf("N%d", level)
}

// grammarConceptStatView is one storage.ConceptStat as the /grammar list
// page's table renders it. LastSeen is the zero time.Time when
// Encounters is 0 (storage.ConceptStat's documented "zero when
// never") — the template renders that as 未遭遇 rather than a
// misleading 0001-01-01 date.
type grammarConceptStatView struct {
	Slug, Name, JLPTChip string
	// Description is the catalog's explanation of the concept, and
	// Example the first of its examples — what the list shows one
	// disclosure deep so that finding out what a concept IS does not
	// cost a page load per concept across a 400-entry catalog. Example
	// is the FIRST only: the row is a taste, and /grammar/{slug} is
	// where the rest of them live.
	Description, Example string
	Encounters           int
	LastSeen             time.Time
}

func toGrammarConceptStatView(cs storage.ConceptStat) grammarConceptStatView {
	v := grammarConceptStatView{
		Slug:        cs.Slug,
		Name:        cs.Name,
		JLPTChip:    jlptChip(cs.JLPTLevel),
		Description: cs.Description,
		Encounters:  cs.Encounters,
		LastSeen:    cs.LastSeen,
	}
	if len(cs.Examples) > 0 {
		v.Example = cs.Examples[0]
	}
	return v
}

// grammarList handles GET /grammar: the full curated JLPT catalog joined
// with the identity's own encounter counts (storage.ConceptStat),
// encountered concepts first — ConceptStats already returns them in that
// order (encounters DESC), so this handler does no re-sorting of its
// own.
func (s *Server) grammarList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	stats, err := s.opts.Grammar.ConceptStats(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load grammar concepts", http.StatusInternalServerError)
		return
	}

	views := make([]grammarConceptStatView, 0, len(stats))
	for _, cs := range stats {
		views = append(views, toGrammarConceptStatView(cs))
	}

	s.render(w, r, "grammar", map[string]any{
		"Title":    "文法",
		"Identity": ident,
		"Concepts": views,
	})
}

// grammarDetail handles GET /grammar/{slug}: the concept's description,
// examples, related/prerequisite chips, and the identity's most recent
// corrections tagged with it. An unknown slug is a 404, not an empty
// page — GetConcept's storage.ErrNotFound is the same sentinel every
// other identity-scoped lookup in this package maps to 404.
func (s *Server) grammarDetail(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	slug := chi.URLParam(r, "slug")

	concept, err := s.opts.Grammar.GetConcept(r.Context(), slug)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load concept", http.StatusInternalServerError)
		return
	}

	corrections, err := s.opts.Grammar.CorrectionsForConcept(r.Context(), ident.ID, slug, grammarConceptCorrectionsLimit)
	if err != nil {
		http.Error(w, "could not load corrections", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "grammar_concept", map[string]any{
		"Title":       concept.Name,
		"Identity":    ident,
		"Concept":     concept,
		"JLPTChip":    jlptChip(concept.JLPTLevel),
		"Corrections": corrections,
	})
}
