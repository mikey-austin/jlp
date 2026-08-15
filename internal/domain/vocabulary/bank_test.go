package vocabulary_test

import (
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

const twoEntryFixture = `
- expression: "それはそれとして"
  reading: ""
  meaning: "that aside; setting that aside for now"
  kind: expression
- expression: "〜に越したことはない"
  reading: "にこしたことはない"
  meaning: "there's nothing better than...; it's best to..."
  kind: pattern
`

func TestLoadBankParsesTwoEntries(t *testing.T) {
	entries, err := vocabulary.LoadBank(strings.NewReader(twoEntryFixture))
	if err != nil {
		t.Fatalf("LoadBank() err = %v, want nil", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	first := entries[0]
	if first.Expression != "それはそれとして" {
		t.Fatalf("entries[0].Expression = %q, want それはそれとして", first.Expression)
	}
	if first.Reading != "" {
		t.Fatalf("entries[0].Reading = %q, want empty", first.Reading)
	}
	if first.Meaning == "" {
		t.Fatal("entries[0].Meaning is empty")
	}
	if first.Kind != vocabulary.KindExpression {
		t.Fatalf("entries[0].Kind = %q, want %q", first.Kind, vocabulary.KindExpression)
	}

	second := entries[1]
	if second.Expression != "〜に越したことはない" {
		t.Fatalf("entries[1].Expression = %q, want 〜に越したことはない", second.Expression)
	}
	if second.Reading != "にこしたことはない" {
		t.Fatalf("entries[1].Reading = %q, want にこしたことはない", second.Reading)
	}
	if second.Kind != vocabulary.KindPattern {
		t.Fatalf("entries[1].Kind = %q, want %q", second.Kind, vocabulary.KindPattern)
	}
}

func TestLoadBankDuplicateExpressionErrors(t *testing.T) {
	const fixture = `
- expression: "それはそれとして"
  meaning: "m1"
  kind: expression
- expression: "それはそれとして"
  meaning: "m2"
  kind: expression
`
	_, err := vocabulary.LoadBank(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadBank() err = nil, want error for duplicate expression")
	}
}

func TestLoadBankEmptyExpressionErrors(t *testing.T) {
	const fixture = `
- expression: ""
  meaning: "m"
  kind: expression
`
	_, err := vocabulary.LoadBank(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadBank() err = nil, want error for empty expression")
	}
}

func TestLoadBankEmptyMeaningErrors(t *testing.T) {
	const fixture = `
- expression: "それはそれとして"
  meaning: ""
  kind: expression
`
	_, err := vocabulary.LoadBank(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadBank() err = nil, want error for empty meaning")
	}
}

func TestLoadBankInvalidKindErrors(t *testing.T) {
	const fixture = `
- expression: "それはそれとして"
  meaning: "m"
  kind: word
`
	_, err := vocabulary.LoadBank(strings.NewReader(fixture))
	if err == nil {
		t.Fatal("LoadBank() err = nil, want error for kind other than expression/pattern")
	}
}
