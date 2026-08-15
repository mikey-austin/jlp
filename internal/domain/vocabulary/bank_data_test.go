package vocabulary_test

import (
	"os"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

// TestLoadBankCoreYAMLIsValid is a scratch check pinning the brief's
// data-authoring requirements against the real file: >=30 entries, and
// PRD §55's four worked examples all present. Kept as a real test (not
// deleted) since it's cheap and guards against a future edit to
// data/expressions/core.yaml silently dropping below the floor or
// losing a required entry.
func TestLoadBankCoreYAMLIsValid(t *testing.T) {
	f, err := os.Open("../../../data/expressions/core.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	entries, err := vocabulary.LoadBank(f)
	if err != nil {
		t.Fatalf("LoadBank(core.yaml) err = %v", err)
	}
	if len(entries) < 30 {
		t.Fatalf("len(entries) = %d, want >= 30", len(entries))
	}

	required := []string{"〜というわけではない", "それはそれとして", "気がしないでもない", "〜に越したことはない"}
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		set[e.Expression] = true
	}
	for _, r := range required {
		if !set[r] {
			t.Errorf("core.yaml missing PRD §55 required expression %q", r)
		}
	}
}
