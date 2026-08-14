// Package grammar holds the curated JLPT grammar concept catalog: the
// reference data behind concept tagging (Phase 2 Task 2), the /grammar
// pages (Task 3), and the spaced-repetition planner (Task 5). Concept
// slugs are the stable identifier every one of those features keys
// off, so LoadCatalog validates them strictly at load time rather than
// letting a typo or duplicate surface later as a silent mismatch.
package grammar

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Concept is one JLPT grammar point: a stable slug (hardcoded into
// agent prompts and test fixtures — see the required-slugs list in
// data/grammar/concepts.yaml) paired with the material a learner-facing
// page or a tagging prompt needs to explain it.
type Concept struct {
	Slug          string   `yaml:"slug"`
	Name          string   `yaml:"name"`
	JLPTLevel     int      `yaml:"jlpt_level"` // 5 (N5, beginner) .. 1 (N1, advanced)
	Description   string   `yaml:"description"`
	Examples      []string `yaml:"examples"`
	Related       []string `yaml:"related"`       // slugs of related (not prerequisite) concepts
	Prerequisites []string `yaml:"prerequisites"` // slugs that should be learned first
}

// LoadCatalog parses a YAML list of Concept entries (see
// data/grammar/concepts.yaml) and validates it before returning:
// every slug must be non-empty and unique across the file, and every
// JLPTLevel must fall in 1..5 (N1..N5). Malformed catalog data fails
// fast here — at load time, in seed and tests — instead of surfacing
// later as a broken UpsertConcepts call or an unexplained /grammar
// page.
func LoadCatalog(r io.Reader) ([]Concept, error) {
	var concepts []Concept
	if err := yaml.NewDecoder(r).Decode(&concepts); err != nil {
		return nil, fmt.Errorf("grammar: decode catalog: %w", err)
	}

	seen := make(map[string]bool, len(concepts))
	for i, c := range concepts {
		if c.Slug == "" {
			return nil, fmt.Errorf("grammar: entry %d: empty slug", i)
		}
		if seen[c.Slug] {
			return nil, fmt.Errorf("grammar: duplicate slug %q", c.Slug)
		}
		seen[c.Slug] = true
		if c.JLPTLevel < 1 || c.JLPTLevel > 5 {
			return nil, fmt.Errorf("grammar: concept %q: jlpt_level %d out of range 1..5", c.Slug, c.JLPTLevel)
		}
	}
	return concepts, nil
}
