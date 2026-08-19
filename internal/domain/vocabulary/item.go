// Package vocabulary holds the learner's personal vocabulary: words,
// expressions, collocations, and grammar patterns looked up while
// reading or writing (PRD §12's ingestion API), tracked through
// lookup → production → successful-production, the same
// encounter-then-recall arc Phase 2's expression bank (Task 7) and
// active-recall drills (Task 8/9) build on.
package vocabulary

import (
	"strings"
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

	// MeaningEN and Tags are populated by POST /api/v1/words (Phase 3
	// Task 8, Nihongo Daily's bulk ingestion contract): an English gloss
	// alongside Meaning's Japanese definition, and free-form tags
	// ("education", "noun", …) the source deck carried. Both are zero
	// value ("" / nil) for items that only ever came from a
	// vocabulary.lookup event — MeaningEN/Tags are never inferred from
	// Meaning/Source.
	MeaningEN string
	Tags      []string

	Lookups               int
	Productions           int
	SuccessfulProductions int

	FirstSeen time.Time
	LastEvent time.Time
}

// MatchableForm is the part of an expression that literally appears in a
// sentence.
//
// A pattern entry is written with a leading (and sometimes trailing)
// 〜 or ～ standing for "whatever comes here": 〜というわけではない is
// never written with the tilde in a real sentence, it is written as
// 嫌いというわけではない. Anything comparing an expression against text
// — finding it, blanking it, emphasising it — has to compare this form,
// or every pattern in the learner's vocabulary silently fails to match.
//
// Returns the expression unchanged when there is no placeholder, which
// is every ordinary word.
func (i Item) MatchableForm() string {
	return MatchableForm(i.Expression)
}

// MatchableForm is Item.MatchableForm for a bare expression.
func MatchableForm(expression string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(expression), "〜～"))
}

// minMatchRunes is the shortest prefix of an expression that may stand
// for it in a sentence. Four is long enough that a match is about this
// expression rather than a common particle sequence, and short enough
// that ordinary inflection still matches.
const minMatchRunes = 4

// MatchIn finds the part of expression that actually appears in
// sentence, and reports whether anything did.
//
// Exact first. Failing that, the longest PREFIX of the expression
// present in the sentence, because a grammar pattern inflects: a
// sentence using 〜というわけではない may well say
// というわけでは*ありません*, and demanding the citation form rejects a
// perfectly correct example. The prefix is what actually appears, so it
// is what can honestly be blanked for a cloze or emphasised in prose —
// returning the citation form instead would mean marking text that is
// not there.
//
// Short prefixes are refused: below minMatchRunes a "match" is a common
// particle string that says nothing about this expression.
func MatchIn(sentence, expression string) (string, bool) {
	form := MatchableForm(expression)
	if form == "" || sentence == "" {
		return "", false
	}
	if strings.Contains(sentence, form) {
		return form, true
	}
	runes := []rune(form)
	for n := len(runes) - 1; n >= minMatchRunes; n-- {
		if prefix := string(runes[:n]); strings.Contains(sentence, prefix) {
			return prefix, true
		}
	}
	return "", false
}
