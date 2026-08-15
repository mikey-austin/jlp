// Package vocabulary holds the learner's personal vocabulary: words,
// expressions, collocations, and grammar patterns looked up while
// reading or writing (PRD §12's ingestion API), tracked through
// lookup → production → successful-production, the same
// encounter-then-recall arc Phase 2's expression bank (Task 7) and
// active-recall drills (Task 8/9) build on.
package vocabulary

import (
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// Kind classifies what an Item's Expression actually is — a single
// word, a multi-word set expression, a fixed collocation, or a
// grammar pattern — distinct from grammar.Concept's JLPT catalog:
// vocabulary is personal and open-ended (anything the learner looks
// up), grammar concepts are curated and fixed.
type Kind string

const (
	KindWord        Kind = "word"
	KindExpression  Kind = "expression"
	KindCollocation Kind = "collocation"
	KindPattern     Kind = "pattern"
)

// Item is one vocabulary entry in the learner's personal catalog: an
// expression they've looked up, plus the running tallies that turn a
// single lookup into a signal of retention — Lookups (how often it's
// been looked up again, a forgetting signal), Productions (how often
// it later showed up in the learner's own writing, detected by
// application/vocabulary.Service.DetectProduction), and
// SuccessfulProductions (the subset of those productions that weren't
// touched by a correction — see DetectProduction's doc comment for
// exactly what "touched" means).
type Item struct {
	ID         string
	IdentityID learner.IdentityID
	Expression string
	Reading    string
	Meaning    string
	Kind       Kind
	JLPTLevel  int    // 0 unknown
	Source     string // free text, e.g. "novel: コンビニ人間"

	Lookups               int
	Productions           int
	SuccessfulProductions int

	FirstSeen time.Time
	LastEvent time.Time
}
