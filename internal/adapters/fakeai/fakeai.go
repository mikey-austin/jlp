// Package fakeai is a deterministic ai.StructuredGenerator: no network
// calls, no API key, same output every time for the same input. It
// exists so the rest of the system — and every test that isn't
// specifically about the Anthropic adapter — can run fully offline
// against a generator that still returns schema-valid JSON.
//
// It recognizes a small, fixed set of Japanese learner mistakes by
// substring match on the rendered user prompt; anything else yields
// zero corrections.
package fakeai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const (
	provider = "fake"
	model    = "fake-1"
)

type explanation struct {
	JA string `json:"ja"`
	EN string `json:"en"`
}

type correction struct {
	Original    string      `json:"original"`
	Replacement string      `json:"replacement"`
	Type        string      `json:"type"`
	Severity    string      `json:"severity"`
	Explanation explanation `json:"explanation"`
	Concepts    []string    `json:"concepts,omitempty"`
}

type correctionResult struct {
	Corrections []correction `json:"corrections"`
}

// iAdjectivePastRules: each entry is a dictionary-form い-adjective the
// fake provider knows learners commonly conjugate wrong as
// "<stem>いでした" instead of "<stem>かったです".
var iAdjectivePastRules = []string{"面白い", "楽しい"}

type generator struct{}

// New returns the fake provider. It satisfies ai.StructuredGenerator.
func New() ai.StructuredGenerator { return &generator{} }

func (g *generator) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()

	result := correctionResult{Corrections: []correction{}}
	for _, adj := range iAdjectivePastRules {
		if c, ok := iAdjectivePastCorrection(adj, req.User); ok {
			result.Corrections = append(result.Corrections, c)
		}
	}
	if strings.Contains(req.User, "を行きます") {
		result.Corrections = append(result.Corrections, correction{
			Original:    "を",
			Replacement: "に",
			Type:        "particle",
			Severity:    "incorrect",
			Explanation: explanation{
				JA: "移動を表す「行きます」は目的地に「に」を使います。「を」は使いません。",
				EN: "行きます (\"to go\") marks its destination with に, not を.",
			},
			Concepts: []string{"particle-ni-destination"},
		})
	}

	payload, err := json.Marshal(result)
	if err != nil {
		return ai.StructuredResponse{}, fmt.Errorf("fakeai: marshal response: %w", err)
	}

	return ai.StructuredResponse{
		JSON:         payload,
		Provider:     provider,
		Model:        model,
		InputTokens:  runeCount(req.System) + runeCount(req.User),
		OutputTokens: runeCount(string(payload)),
		Latency:      time.Since(start),
	}, nil
}

// iAdjectivePastCorrection reports the deterministic correction for
// dictionary-form い-adjective adj (e.g. "面白い") if the wrong past
// tense "<stem>いでした" appears in text.
func iAdjectivePastCorrection(adj, text string) (correction, bool) {
	stem := strings.TrimSuffix(adj, "い")
	wrong := stem + "いでした"
	right := stem + "かったです"
	if !strings.Contains(text, wrong) {
		return correction{}, false
	}
	return correction{
		Original:    wrong,
		Replacement: right,
		Type:        "conjugation",
		Severity:    "incorrect",
		Explanation: explanation{
			JA: fmt.Sprintf("い形容詞の過去形は「〜かった」を使います。「%s」→「%s」。", adj, stem+"かった"),
			EN: fmt.Sprintf("い-adjectives form the past tense with 〜かった, so %s must be %s.", wrong, right),
		},
		Concepts: []string{"i-adjective-past"},
	}, true
}

func runeCount(s string) int { return len([]rune(s)) }
