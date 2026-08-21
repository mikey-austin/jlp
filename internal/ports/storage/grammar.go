package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// ConceptStat is one grammar_concepts row (reference data) joined with
// how often ONE identity's corrections have been tagged with it — the
// input to a "your weak points" view. Encounters and LastSeen are
// always scoped to the identity passed to
// GrammarRepository.ConceptStats: a concept only ever tagged by
// another identity's corrections still appears here, with Encounters
// 0 and LastSeen zero, never omitted — see db/queries/grammar.sql's
// ConceptStats query for how that isolation is enforced.
type ConceptStat struct {
	Slug, Name string
	JLPTLevel  int
	// Description and Examples are the catalog's own explanation of the
	// concept, carried here so /grammar can show what a concept IS
	// without a page load per concept — the list is the whole catalog,
	// and a name plus a level does not tell a learner whether they know
	// it. Same columns GetConcept returns; no extra query.
	Description string
	Examples    []string
	Encounters  int       // corrections tagged with this concept for the identity
	LastSeen    time.Time // zero when never
}

// GrammarRepository persists the curated JLPT concept catalog
// (reference data, loaded via UpsertConcepts from
// data/grammar/concepts.yaml — see cmd/jlp/seed.go) and answers how one
// identity's corrections relate to it. ListConcepts and GetConcept are
// NOT identity-scoped — every learner sees the same catalog.
// ConceptStats and CorrectionsForConcept ARE identity-scoped, mirroring
// the access-control shape every other identity-scoped repository in
// this package uses.
type GrammarRepository interface {
	UpsertConcepts(ctx context.Context, cs []grammar.Concept) error
	ListConcepts(ctx context.Context) ([]grammar.Concept, error)          // reference data, NOT identity-scoped
	GetConcept(ctx context.Context, slug string) (grammar.Concept, error) // ErrNotFound
	ConceptStats(ctx context.Context, identity learner.IdentityID) ([]ConceptStat, error)
	CorrectionsForConcept(ctx context.Context, identity learner.IdentityID, slug string, limit int) ([]CorrectionRecord, error)
}
