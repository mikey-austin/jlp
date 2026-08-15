package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	priorityDefaultLimit   = 5
	priorityMaxLimit       = 20
	grammarHistoryMaxLimit = 30
)

// AnalyticsTools returns the "analytics" group's tools (PRD §29):
// get_learning_priorities and get_grammar_history — both aggregate
// views the planner/grammar repositories already maintain, delegating
// directly to them (PriorityRepository.Top, GrammarRepository.
// ConceptStats).
func AnalyticsTools(priorities storage.PriorityRepository, grammar storage.GrammarRepository) []Tool {
	return []Tool{
		getLearningPrioritiesTool(priorities),
		getGrammarHistoryTool(grammar),
	}
}

type priorityView struct {
	SubjectType string  `json:"subject_type"`
	Subject     string  `json:"subject"`
	Score       float64 `json:"score"`
	Reason      string  `json:"reason"`
}

type learningPrioritiesArgs struct {
	Limit int `json:"limit"`
}

func getLearningPrioritiesTool(priorities storage.PriorityRepository) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_learning_priorities",
			Description: "Returns the teaching planner's highest-scoring priorities for the learner (what to focus on next, and why), score descending. Optional limit (default 5, max 20).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[learningPrioritiesArgs](args)
			if err != nil {
				return "", fmt.Errorf("get_learning_priorities: %w", err)
			}
			limit := clampLimit(a.Limit, priorityDefaultLimit, priorityMaxLimit)
			ps, err := priorities.Top(ctx, identity, limit)
			if err != nil {
				return "", fmt.Errorf("get_learning_priorities: %w", err)
			}
			views := make([]priorityView, 0, len(ps))
			for _, p := range ps {
				views = append(views, priorityView{
					SubjectType: p.SubjectType,
					Subject:     p.Subject,
					Score:       p.Score,
					Reason:      p.Reason,
				})
			}
			out, err := json.Marshal(views)
			if err != nil {
				return "", fmt.Errorf("get_learning_priorities: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

type conceptStatView struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	JLPTLevel  int    `json:"jlpt_level"`
	Encounters int    `json:"encounters"`
}

func getGrammarHistoryTool(grammar storage.GrammarRepository) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_grammar_history",
			Description: "Returns the learner's grammar-concept encounter history from the JLPT catalog (how often each concept has come up in their corrections), most-encountered first, capped at 30. Takes no arguments.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, _ json.RawMessage) (string, error) {
			stats, err := grammar.ConceptStats(ctx, identity)
			if err != nil {
				return "", fmt.Errorf("get_grammar_history: %w", err)
			}
			sort.Slice(stats, func(i, j int) bool { return stats[i].Encounters > stats[j].Encounters })
			if len(stats) > grammarHistoryMaxLimit {
				stats = stats[:grammarHistoryMaxLimit]
			}
			views := make([]conceptStatView, 0, len(stats))
			for _, s := range stats {
				views = append(views, conceptStatView{
					Slug:       s.Slug,
					Name:       s.Name,
					JLPTLevel:  s.JLPTLevel,
					Encounters: s.Encounters,
				})
			}
			out, err := json.Marshal(views)
			if err != nil {
				return "", fmt.Errorf("get_grammar_history: encode: %w", err)
			}
			return string(out), nil
		},
	}
}
