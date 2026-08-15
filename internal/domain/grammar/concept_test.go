package grammar_test

import (
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/grammar"
)

const twoConceptFixture = `
- slug: te-form
  name: "て-form (〜て)"
  jlpt_level: 5
  description: "Connects verbs/clauses, forms requests, and links to other te-form constructions."
  examples:
    - "朝ごはんを食べて、学校に行きます。"
    - "ちょっと待ってください。"
  related: ["past-tense-plain"]
  prerequisites: []
- slug: past-tense-plain
  name: "plain past tense (〜た)"
  jlpt_level: 5
  description: "The plain (dictionary-style) past tense form of verbs and い-adjectives."
  examples:
    - "昨日、映画を見た。"
    - "去年、日本に行った。"
  related: []
  prerequisites: ["te-form"]
`

func TestLoadCatalogParsesTwoConcepts(t *testing.T) {
	concepts, err := grammar.LoadCatalog(strings.NewReader(twoConceptFixture))
	if err != nil {
		t.Fatalf("LoadCatalog() err = %v, want nil", err)
	}
	if len(concepts) != 2 {
		t.Fatalf("len(concepts) = %d, want 2", len(concepts))
	}

	first := concepts[0]
	if first.Slug != "te-form" {
		t.Fatalf("concepts[0].Slug = %q, want te-form", first.Slug)
	}
	if first.Name != "て-form (〜て)" {
		t.Fatalf("concepts[0].Name = %q, want て-form (〜て)", first.Name)
	}
	if first.JLPTLevel != 5 {
		t.Fatalf("concepts[0].JLPTLevel = %d, want 5", first.JLPTLevel)
	}
	if first.Description == "" {
		t.Fatal("concepts[0].Description is empty")
	}
	if len(first.Examples) != 2 {
		t.Fatalf("len(concepts[0].Examples) = %d, want 2", len(first.Examples))
	}
	if len(first.Related) != 1 || first.Related[0] != "past-tense-plain" {
		t.Fatalf("concepts[0].Related = %v, want [past-tense-plain]", first.Related)
	}
	if len(first.Prerequisites) != 0 {
		t.Fatalf("concepts[0].Prerequisites = %v, want empty", first.Prerequisites)
	}

	second := concepts[1]
	if second.Slug != "past-tense-plain" {
		t.Fatalf("concepts[1].Slug = %q, want past-tense-plain", second.Slug)
	}
	if len(second.Prerequisites) != 1 || second.Prerequisites[0] != "te-form" {
		t.Fatalf("concepts[1].Prerequisites = %v, want [te-form]", second.Prerequisites)
	}
}

func TestLoadCatalogDuplicateSlugErrors(t *testing.T) {
	const fixture = `
- slug: te-form
  name: "て-form"
  jlpt_level: 5
  description: "d"
  examples: ["a", "b"]
- slug: te-form
  name: "て-form again"
  jlpt_level: 5
  description: "d"
  examples: ["a", "b"]
`
	_, err := grammar.LoadCatalog(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadCatalog() err = nil, want error for duplicate slug")
	}
}

func TestLoadCatalogLevelOutOfRangeErrors(t *testing.T) {
	const fixture = `
- slug: bad-level
  name: "bad"
  jlpt_level: 0
  description: "d"
  examples: ["a", "b"]
`
	_, err := grammar.LoadCatalog(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadCatalog() err = nil, want error for jlpt_level 0")
	}
}

func TestLoadCatalogEmptySlugErrors(t *testing.T) {
	const fixture = `
- slug: ""
  name: "no slug"
  jlpt_level: 3
  description: "d"
  examples: ["a", "b"]
`
	_, err := grammar.LoadCatalog(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadCatalog() err = nil, want error for empty slug")
	}
}

// TestLoadCatalogDanglingRelatedErrors pins the cross-ref validation
// (Phase 3 Task 1, carried over from Phase 2's final review): a
// `related` slug that names no concept in the catalog must fail to
// load, naming the dangling ref in the error — a typo here would
// otherwise only surface later as a 404 when a learner clicks the
// resulting chip on /grammar/{slug} (see concept.go's package doc
// comment).
func TestLoadCatalogDanglingRelatedErrors(t *testing.T) {
	const fixture = `
- slug: te-form
  name: "て-form"
  jlpt_level: 5
  description: "d"
  examples: ["a", "b"]
  related: ["no-such-concept"]
`
	_, err := grammar.LoadCatalog(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadCatalog() err = nil, want error for dangling related ref")
	}
	if !strings.Contains(err.Error(), "no-such-concept") {
		t.Errorf("LoadCatalog() err = %v, want it to name the dangling ref %q", err, "no-such-concept")
	}
}

// TestLoadCatalogDanglingPrerequisiteErrors mirrors the above for
// `prerequisites` — the two fields are validated the same way, but
// pinned separately so a future change that only wires up one of them
// can't hide behind the other's passing test.
func TestLoadCatalogDanglingPrerequisiteErrors(t *testing.T) {
	const fixture = `
- slug: past-tense-plain
  name: "plain past tense"
  jlpt_level: 5
  description: "d"
  examples: ["a", "b"]
  prerequisites: ["ghost-concept"]
`
	_, err := grammar.LoadCatalog(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadCatalog() err = nil, want error for dangling prerequisite ref")
	}
	if !strings.Contains(err.Error(), "ghost-concept") {
		t.Errorf("LoadCatalog() err = %v, want it to name the dangling ref %q", err, "ghost-concept")
	}
}

// TestLoadCatalogValidCrossRefsPass is the positive counterpart: refs
// that DO resolve to another slug in the same catalog must not be
// rejected by the new validation (guards against an overzealous
// implementation, e.g. one that rejects every non-empty related list).
func TestLoadCatalogValidCrossRefsPass(t *testing.T) {
	_, err := grammar.LoadCatalog(strings.NewReader(twoConceptFixture))
	if err != nil {
		t.Fatalf("LoadCatalog() err = %v, want nil (both refs resolve within the fixture)", err)
	}
}
