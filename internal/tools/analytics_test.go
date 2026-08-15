package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
	"github.com/mikeyaustin/jlp/internal/tools"
)

func TestGetLearningPrioritiesDelegatesAndIsIdentityScoped(t *testing.T) {
	prios := newFakePriorityRepo()
	if err := prios.ReplaceAll(context.Background(), testIdentity, []storage.Priority{
		{IdentityID: testIdentity, SubjectType: "concept", Subject: "i-adjective-past", Score: 5, Reason: "recurring"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := prios.ReplaceAll(context.Background(), otherIdentity, []storage.Priority{
		{IdentityID: otherIdentity, SubjectType: "concept", Subject: "not-yours", Score: 9},
	}); err != nil {
		t.Fatal(err)
	}

	tool := findTool(t, tools.AnalyticsTools(prios, newFakeGrammarRepo(nil)), "get_learning_priorities")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 || got[0]["subject"] != "i-adjective-past" {
		t.Fatalf("got %v, want exactly testIdentity's own priority: output=%s", got, out)
	}
}

func TestGetGrammarHistorySortsByEncountersDescending(t *testing.T) {
	repo := newFakeGrammarRepo([]grammar.Concept{{Slug: "a", Name: "A"}, {Slug: "b", Name: "B"}})
	repo.statsByID[testIdentity] = []storage.ConceptStat{
		{Slug: "a", Name: "A", Encounters: 2},
		{Slug: "b", Name: "B", Encounters: 9},
	}

	tool := findTool(t, tools.AnalyticsTools(newFakePriorityRepo(), repo), "get_grammar_history")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 2 || got[0]["slug"] != "b" {
		t.Fatalf("got %v, want [b a] (encounters descending): output=%s", got, out)
	}
}
