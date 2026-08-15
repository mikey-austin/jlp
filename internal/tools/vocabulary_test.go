package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // port-shaped test double; see fakes_test.go's package doc comment
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/tools"
)

func newVocabService(items []vocabulary.Item) *appvocabulary.Service {
	repo := &fakeVocabRepo{items: items}
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	return appvocabulary.NewService(repo, rec)
}

func TestGetVocabularyHistoryDelegatesAndIsIdentityScoped(t *testing.T) {
	items := []vocabulary.Item{
		{ID: "1", IdentityID: testIdentity, Expression: "それはそれとして", Meaning: "that aside", Kind: vocabulary.KindExpression, Lookups: 3},
		{ID: "2", IdentityID: otherIdentity, Expression: "not-yours", Kind: vocabulary.KindWord},
	}
	svc := newVocabService(items)
	tool := findTool(t, tools.VocabularyTools(svc), "get_vocabulary_history")

	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 || got[0]["expression"] != "それはそれとして" {
		t.Fatalf("got %v, want exactly the one item belonging to testIdentity: output=%s", got, out)
	}
}

func TestGetVocabularyHistoryCapsAtLimit(t *testing.T) {
	var items []vocabulary.Item
	for i := 0; i < 60; i++ {
		items = append(items, vocabulary.Item{ID: string(rune('a' + i%26)), IdentityID: testIdentity, Expression: "word", Kind: vocabulary.KindWord})
	}
	svc := newVocabService(items)
	tool := findTool(t, tools.VocabularyTools(svc), "get_vocabulary_history")

	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("len(got) = %d, want 20 (the default cap, well under the 60 available) — tool outputs must stay compact", len(got))
	}
}

func TestSearchVocabularyMatchesSubstring(t *testing.T) {
	items := []vocabulary.Item{
		{ID: "1", IdentityID: testIdentity, Expression: "面白い", Meaning: "interesting"},
		{ID: "2", IdentityID: testIdentity, Expression: "楽しい", Meaning: "fun"},
	}
	svc := newVocabService(items)
	tool := findTool(t, tools.VocabularyTools(svc), "search_vocabulary")

	out, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"query":"interesting"}`))
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 || got[0]["expression"] != "面白い" {
		t.Fatalf("got %v, want exactly 面白い to match \"interesting\": output=%s", got, out)
	}
}

func TestSearchVocabularyRequiresQuery(t *testing.T) {
	svc := newVocabService(nil)
	tool := findTool(t, tools.VocabularyTools(svc), "search_vocabulary")

	_, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Handler() err = nil, want an error when query is missing")
	}
}
