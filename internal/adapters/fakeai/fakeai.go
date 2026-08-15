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

	// schemaV1/schemaV2 are the only schemas the fake provider knows how
	// to produce (see supportedSchemas below). A request for anything
	// else is a caller bug (a new capability wired up without teaching
	// the fake provider its shape) and must fail loudly rather than
	// silently return correction-result JSON that doesn't match what was
	// asked for.
	schemaV1 = "correction_result.v1"
	schemaV2 = "correction_result.v2"

	// socraticMarker is the EXACT line internal/agent/teacher's
	// teacher.feedback.v3 USER template renders when — and only when —
	// the session's TeacherMode is "socratic" (that template's opening
	// "Teacher mode: {{.TeacherMode}}" line; see its doc comment). This
	// must be the full "Teacher mode: socratic" phrase, not a bare
	// "socratic" substring: a bare substring would also match the word
	// "socratic" appearing anywhere else in User — including inside
	// session.Session.Purpose, learner-supplied free text rendered
	// verbatim into the SAME template's "Session purpose: {{.Purpose}}"
	// line (e.g. a "teacher"-mode session whose Purpose happens to be
	// "practicing the socratic method" would otherwise get hints
	// attached despite not being in socratic mode at all) — and it
	// cannot collide with the v3 SYSTEM template's own, always-present
	// "Teacher mode 'socratic': ..." hint instruction either (different
	// punctuation — quote vs colon right after "mode" — though that
	// sentence living in System, never checked here, already rules it
	// out on its own).
	socraticMarker = "Teacher mode: socratic"
)

// supportedSchemas is the set schemaV1/schemaV2 above name; presented as
// a set so GenerateStructured's guard reads as one membership test
// rather than an OR chain that grows every time a schema is added.
var supportedSchemas = map[string]bool{schemaV1: true, schemaV2: true}

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
	// Hint is only ever set when the caller asked for schemaV2 AND the
	// rendered prompt carries socraticMarker (see hintFor below) —
	// schemaV1's item schema is additionalProperties:false with no
	// "hint" property, so attaching one to a v1 response would produce
	// JSON that fails its own schema validation.
	Hint *explanation `json:"hint,omitempty"`
}

type correctionResult struct {
	Corrections []correction `json:"corrections"`
}

// iAdjectivePastRules: each entry is a dictionary-form い-adjective the
// fake provider knows learners commonly conjugate wrong as
// "<stem>いでした" instead of "<stem>かったです".
var iAdjectivePastRules = []string{"面白い", "楽しい"}

// iAdjectivePastHint/particleHint are the fake provider's socratic-mode
// hints (Phase 2 Task 8, PRD §9/§53): a nudge that names the problem
// area WITHOUT giving away the corrected form. iAdjectivePastHint's
// text is the brief's exact pinned wording — it's rule-general (not
// tied to which い-adjective matched), so it's shared by every
// iAdjectivePastRules entry.
var (
	iAdjectivePastHint = explanation{
		JA: "い形容詞の過去形の作り方を思い出してください。",
		EN: "Recall how い-adjectives form the past tense.",
	}
	particleHint = explanation{
		JA: "「行きます」と一緒に使う助詞を思い出してください。",
		EN: "Recall which particle pairs with 行きます.",
	}
)

// hintFor returns a pointer to hint when socratic is true, nil
// otherwise — the single place GenerateStructured's two rules decide
// whether to attach a hint, so that decision can't drift between them.
func hintFor(socratic bool, hint explanation) *explanation {
	if !socratic {
		return nil
	}
	h := hint
	return &h
}

type generator struct{}

// New returns the fake provider. It satisfies ai.StructuredGenerator.
func New() ai.StructuredGenerator { return &generator{} }

func (g *generator) GenerateStructured(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()

	if req.SchemaName != "" && !supportedSchemas[req.SchemaName] {
		return ai.StructuredResponse{Provider: provider, Model: model},
			fmt.Errorf("fakeai: schema %q not supported (only %q, %q)", req.SchemaName, schemaV1, schemaV2)
	}

	// socratic gates hint attachment on BOTH conditions schemaV2's own
	// doc comment describes: the caller requested the schema that has a
	// "hint" property to put one in, AND the rendered prompt actually
	// carries socraticMarker for THIS request (a v2-schema request for a
	// non-socratic session must still come back hint-less).
	socratic := req.SchemaName == schemaV2 && strings.Contains(req.User, socraticMarker)

	result := correctionResult{Corrections: []correction{}}
	for _, adj := range iAdjectivePastRules {
		if c, ok := iAdjectivePastCorrection(adj, req.User); ok {
			c.Hint = hintFor(socratic, iAdjectivePastHint)
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
			Concepts: []string{"particle-ni-direction"},
			Hint:     hintFor(socratic, particleHint),
		})
	}

	payload, err := json.Marshal(result)
	if err != nil {
		// Provider/Model are known regardless of outcome, so set them
		// even here: the observability decorator's audit record must be
		// able to say which provider/model a failed call went through.
		return ai.StructuredResponse{Provider: provider, Model: model},
			fmt.Errorf("fakeai: marshal response: %w", err)
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
