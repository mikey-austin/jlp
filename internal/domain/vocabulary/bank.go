package vocabulary

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// BankEntry is one curated expression-bank entry (see
// data/expressions/core.yaml): a set expression or grammar pattern the
// learner is expected to already understand receptively, seeded as a
// zero-count Item baseline so PRD §55/§17.5's activator has something
// to encourage even before the learner has ever looked one up
// themselves — see cmd/jlp/seed.go's seedExpressionBank and
// storage.VocabularyRepository.SeedBankItem for how this loads into
// vocabulary_items without disturbing an existing item's counts.
type BankEntry struct {
	Expression string `yaml:"expression"`
	Reading    string `yaml:"reading"` // hiragana reading; "" when Expression is already pure kana
	Meaning    string `yaml:"meaning"`
	Kind       Kind   `yaml:"kind"` // must be KindExpression or KindPattern — see LoadBank
}

// LoadBank parses a YAML list of BankEntry (see
// data/expressions/core.yaml) and validates it before returning: every
// Expression must be non-empty and unique across the file, every
// Meaning must be non-empty, and every Kind must be KindExpression or
// KindPattern — the bank is deliberately narrower than the full
// vocabulary.Kind enum (no KindWord/KindCollocation entries; those
// come from the learner's own lookups, not the curated bank). Malformed
// bank data fails fast here — at load time, in seed and tests — rather
// than surfacing later as a silently-empty expression bank.
func LoadBank(r io.Reader) ([]BankEntry, error) {
	var entries []BankEntry
	if err := yaml.NewDecoder(r).Decode(&entries); err != nil {
		return nil, fmt.Errorf("vocabulary: decode bank: %w", err)
	}

	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if e.Expression == "" {
			return nil, fmt.Errorf("vocabulary: bank entry %d: empty expression", i)
		}
		if seen[e.Expression] {
			return nil, fmt.Errorf("vocabulary: bank: duplicate expression %q", e.Expression)
		}
		seen[e.Expression] = true
		if e.Meaning == "" {
			return nil, fmt.Errorf("vocabulary: bank entry %q: empty meaning", e.Expression)
		}
		if e.Kind != KindExpression && e.Kind != KindPattern {
			return nil, fmt.Errorf("vocabulary: bank entry %q: kind %q must be %q or %q", e.Expression, e.Kind, KindExpression, KindPattern)
		}
	}
	return entries, nil
}
