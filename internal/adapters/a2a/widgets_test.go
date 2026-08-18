package a2a

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
)

// TestGatedCorrectionNeverReachesADataPart is the ninth-surface guard.
//
// domain/correction.IsGated has leaked through eight surfaces in this
// project's history. Rendering is a ninth, and the only reason this one
// is safe is that widgets copy tool results, which internal/tools has
// already run through redactIfGated — a gated correction arrives with no
// replacement and no explanation.
//
// The failure this pins is a specific, plausible future change: someone
// "improves" the widget by fetching the correction from the repository
// to recover the explanation the payload is missing, and hands the
// learner the answer they were supposed to work out.
func TestGatedCorrectionNeverReachesADataPart(t *testing.T) {
	// Exactly what internal/tools emits for a gated correction: the
	// answer-bearing fields are already blank.
	gated := `[{"id":"c1","original":"昨日、映画を見行った","type":"particle","severity":"minor","status":"presented","attempts":2}]`

	parts := widgetParts([]agentrun.ToolCall{{Name: "get_recent_errors", Result: gated}}, acceptsEverything)
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}

	encoded, err := json.Marshal(parts[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)

	// No replacement, no explanation, and — the part that is easy to get
	// wrong — no spans. Spans are computed from the replacement, so a
	// diff against an empty string would mark the whole sentence deleted:
	// wrong on its face, and a tell that something was withheld.
	for _, leak := range []string{"replacement", "explanation_en", "spans"} {
		if strings.Contains(body, leak) {
			t.Errorf("a gated correction's data part carries %q, handing the learner an answer they have not earned:\n%s", leak, body)
		}
	}
	// The gated state still has to be renderable, or the widget cannot
	// say "this is where the problem is, the answer is still open".
	for _, needed := range []string{"original", "severity", "attempts"} {
		if !strings.Contains(body, needed) {
			t.Errorf("the data part dropped %q, so the gated state cannot be rendered:\n%s", needed, body)
		}
	}
}

func TestUngatedCorrectionCarriesSpans(t *testing.T) {
	ungated := `[{"id":"c1","original":"映画を見行った","replacement":"映画を見に行った","type":"particle","severity":"minor","explanation_en":"needs に","status":"accepted"}]`

	parts := widgetParts([]agentrun.ToolCall{{Name: "get_correction_history", Result: ungated}}, acceptsEverything)
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	if parts[0].MediaType != mediaCorrection {
		t.Errorf("mediaType = %q, want %q", parts[0].MediaType, mediaCorrection)
	}

	payload, ok := parts[0].Data.([]correctionPayload)
	if !ok {
		t.Fatalf("data is %T, want []correctionPayload", parts[0].Data)
	}
	if len(payload[0].Spans) == 0 {
		t.Fatal("no spans, so the client would have to diff Japanese itself — the whole reason spans exist")
	}

	// The diff must actually describe the change, not just exist.
	var inserted string
	for _, s := range payload[0].Spans {
		if s.Op == "insert" {
			inserted += s.Text
		}
	}
	if !strings.Contains(inserted, "に") {
		t.Errorf("the diff does not mark に as inserted; spans = %+v", payload[0].Spans)
	}
}

// An agent narrowing down calls the same tool repeatedly. The reader
// wants the view it settled on, not three stacked widgets.
func TestRepeatedToolCallsCollapseToTheLastOne(t *testing.T) {
	calls := []agentrun.ToolCall{
		{Name: "get_vocabulary_history", Result: `[{"expression":"first"}]`},
		{Name: "get_vocabulary_history", Result: `[{"expression":"second"}]`},
		{Name: "get_vocabulary_history", Result: `[{"expression":"third"}]`},
	}
	parts := widgetParts(calls, acceptsEverything)
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	encoded, _ := json.Marshal(parts[0].Data)
	if !strings.Contains(string(encoded), "third") {
		t.Errorf("kept the wrong call: %s", encoded)
	}
}

func TestNonRenderableAndFailedToolsProduceNoWidget(t *testing.T) {
	cases := []struct {
		name string
		call agentrun.ToolCall
	}{
		{"tool with no widget", agentrun.ToolCall{Name: "record_learning_event", Result: `{"ok":true}`}},
		{"failed tool", agentrun.ToolCall{Name: "get_recent_errors", Result: "tool refused: not permitted", IsError: true}},
		{"result that is not JSON", agentrun.ToolCall{Name: "get_recent_errors", Result: "not json at all"}},
		{"empty list", agentrun.ToolCall{Name: "get_vocabulary_history", Result: `[]`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if parts := widgetParts([]agentrun.ToolCall{c.call}, acceptsEverything); len(parts) != 0 {
				t.Errorf("got %d parts, want none: %+v", len(parts), parts)
			}
		})
	}
}

// Order follows the agent's own working order, so the widgets read in
// the sequence it gathered them.
func TestWidgetsKeepFirstAppearanceOrder(t *testing.T) {
	parts := widgetParts([]agentrun.ToolCall{
		{Name: "get_learning_priorities", Result: `[{"subject":"は/が"}]`},
		{Name: "get_vocabulary_history", Result: `[{"expression":"読書"}]`},
		{Name: "get_learning_priorities", Result: `[{"subject":"は/が"}]`},
	}, acceptsEverything)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if parts[0].MediaType != mediaPriorities || parts[1].MediaType != mediaVocabulary {
		t.Errorf("order = %q, %q; want priorities then vocabulary", parts[0].MediaType, parts[1].MediaType)
	}
}

// acceptsEverything is the configuration a client sends when it can
// render all four widget types — the case these tests are about. What
// happens when it accepts less is TestClientOnlyGetsWhatItAccepts'
// business.
var acceptsEverything = &sendMessageConfiguration{AcceptedOutputModes: []string{
	textMode, mediaCorrection, mediaVocabulary, mediaPriorities, mediaLesson,
}}
