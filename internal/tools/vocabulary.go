package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const (
	vocabHistoryDefaultLimit = 20
	vocabHistoryMaxLimit     = 50
	searchVocabDefaultLimit  = 10
	searchVocabMaxLimit      = 25
)

// vocabItemView is the compact, model-facing shape both vocabulary
// tools below return — a trimmed projection of vocabulary.Item.
type vocabItemView struct {
	Expression  string `json:"expression"`
	Reading     string `json:"reading,omitempty"`
	Meaning     string `json:"meaning,omitempty"`
	Kind        string `json:"kind"`
	Lookups     int    `json:"lookups"`
	Productions int    `json:"productions"`
}

func toVocabItemView(it vocabulary.Item) vocabItemView {
	return vocabItemView{
		Expression:  it.Expression,
		Reading:     it.Reading,
		Meaning:     it.Meaning,
		Kind:        string(it.Kind),
		Lookups:     it.Lookups,
		Productions: it.Productions,
	}
}

// VocabularyTools returns the "vocabulary" group's tools (PRD §29):
// get_vocabulary_history and search_vocabulary, both delegating to
// application/vocabulary.Service.List — search_vocabulary's substring
// filter is the one bit of logic that isn't a pure pass-through, but
// it's presentation filtering over an already-fetched list, not a new
// application/domain capability (no state read/written beyond List).
func VocabularyTools(svc *appvocabulary.Service) []Tool {
	return []Tool{
		getVocabularyHistoryTool(svc),
		searchVocabularyTool(svc),
	}
}

type vocabHistoryArgs struct {
	Filter string `json:"filter"`
	Limit  int    `json:"limit"`
}

func getVocabularyHistoryTool(svc *appvocabulary.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_vocabulary_history",
			Description: `Returns the learner's personal vocabulary, most recently active first. Optional filter: "all" (default), "looked-up", "produced", or "activate" (items ready for encouragement). Optional limit (default 20, max 50).`,
			// "all" rather than "": Gemini rejects a request outright when
			// any enum member is empty ("enum[0]: cannot be empty"), which
			// took down every agentic call routed to it. Naming the default
			// is better prompting anyway — a model should not have to infer
			// that the empty string means everything.
			Schema: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string","enum":["all","looked-up","produced","activate"]},"limit":{"type":"integer"}},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[vocabHistoryArgs](args)
			if err != nil {
				return "", fmt.Errorf("get_vocabulary_history: %w", err)
			}
			items, err := svc.List(ctx, identity, vocabFilterFromTool(a.Filter))
			if err != nil {
				return "", fmt.Errorf("get_vocabulary_history: %w", err)
			}
			limit := clampLimit(a.Limit, vocabHistoryDefaultLimit, vocabHistoryMaxLimit)
			if len(items) > limit {
				items = items[:limit]
			}
			views := make([]vocabItemView, 0, len(items))
			for _, it := range items {
				views = append(views, toVocabItemView(it))
			}
			out, err := json.Marshal(views)
			if err != nil {
				return "", fmt.Errorf("get_vocabulary_history: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

type searchVocabularyArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func searchVocabularyTool(svc *appvocabulary.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "search_vocabulary",
			Description: "Searches the learner's personal vocabulary by substring match against expression, reading, or meaning. query is required. Optional limit (default 10, max 25).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"],"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[searchVocabularyArgs](args)
			if err != nil {
				return "", fmt.Errorf("search_vocabulary: %w", err)
			}
			if strings.TrimSpace(a.Query) == "" {
				return "", fmt.Errorf("search_vocabulary: query is required")
			}
			items, err := svc.List(ctx, identity, "")
			if err != nil {
				return "", fmt.Errorf("search_vocabulary: %w", err)
			}
			limit := clampLimit(a.Limit, searchVocabDefaultLimit, searchVocabMaxLimit)
			views := make([]vocabItemView, 0, limit)
			for _, it := range items {
				if len(views) >= limit {
					break
				}
				if matchesVocabQuery(it, a.Query) {
					views = append(views, toVocabItemView(it))
				}
			}
			out, err := json.Marshal(views)
			if err != nil {
				return "", fmt.Errorf("search_vocabulary: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

func matchesVocabQuery(it vocabulary.Item, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(it.Expression), q) ||
		strings.Contains(strings.ToLower(it.Reading), q) ||
		strings.Contains(strings.ToLower(it.Meaning), q)
}

// vocabFilterFromTool maps the tool's filter vocabulary onto the
// application layer's, where "everything" is spelled "".
//
// An unset filter and "all" both mean everything, so a model that omits
// the argument and one that names the default get the same answer.
// Anything else passes through untouched: an unknown value must reach
// the application layer and be handled there, not be silently rewritten
// into "everything" here.
func vocabFilterFromTool(filter string) string {
	if filter == "all" {
		return ""
	}
	return filter
}
