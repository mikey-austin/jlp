package a2a

import (
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
)

// Every widget-bearing call a test in this file makes.
func gatheringRun() []agentrun.ToolCall {
	return []agentrun.ToolCall{
		{Name: "get_recent_errors", Result: `[{"id":"c1","original":"寒いでした","replacement":"寒かったです","type":"conjugation","severity":"major"}]`},
		{Name: "get_vocabulary_history", Result: `[{"expression":"読書","reading":"どくしょ"}]`},
		{Name: "get_learning_priorities", Result: `[{"subject":"は/が","score":1.0}]`},
	}
}

// A client is sent only what it said it can render. Anything else is a
// payload it can at best display as a JSON blob — and, worse, would make
// the prose's deference to a card unsafe, because the card never arrives.
func TestClientOnlyGetsWhatItAccepts(t *testing.T) {
	cfg := &sendMessageConfiguration{AcceptedOutputModes: []string{textMode, mediaCorrection}}

	parts := widgetParts(gatheringRun(), cfg)
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1 (only the accepted correction): %+v", len(parts), parts)
	}
	if parts[0].MediaType != mediaCorrection {
		t.Errorf("mediaType = %q, want %q", parts[0].MediaType, mediaCorrection)
	}
}

// Silence means "the default", and the card's defaultOutputModes says
// that is text/plain. A stranger integrating against this agent must not
// start receiving JLP-shaped payloads it never asked for.
func TestSilentClientGetsProseOnly(t *testing.T) {
	for _, name := range []string{"no configuration", "empty list"} {
		t.Run(name, func(t *testing.T) {
			var cfg *sendMessageConfiguration
			if name == "empty list" {
				cfg = &sendMessageConfiguration{}
			}
			if parts := widgetParts(gatheringRun(), cfg); len(parts) != 0 {
				t.Errorf("a client that asked for nothing got %d data parts: %+v", len(parts), parts)
			}
		})
	}
}

// The rendering note is what stops the model writing out the very list
// being drawn beneath it. It must name only what will actually appear.
func TestRenderingNoteNamesOnlyAcceptedTypes(t *testing.T) {
	cfg := &sendMessageConfiguration{AcceptedOutputModes: []string{textMode, mediaPriorities}}

	system := withRenderingNote("BASE PROMPT", cfg)
	if !strings.Contains(system, "BASE PROMPT") {
		t.Fatal("the note replaced the prompt instead of appending to it")
	}
	if !strings.Contains(system, "priorities") {
		t.Errorf("the note does not mention the priorities chart the client will render:\n%s", system)
	}
	if strings.Contains(system, "vocabulary") {
		t.Errorf("the note promises vocabulary cards the client did not accept, so the prose may defer to something that never appears:\n%s", system)
	}
}

// A client that renders nothing must get its prompt untouched. Telling
// that agent to "refer to the cards below" would produce an answer
// pointing at something the reader cannot see — strictly worse than the
// duplication this whole change is removing.
func TestNoRenderingNoteForAClientThatRendersNothing(t *testing.T) {
	for _, cfg := range []*sendMessageConfiguration{
		nil,
		{},
		{AcceptedOutputModes: []string{textMode}},
	} {
		if got := withRenderingNote("BASE PROMPT", cfg); got != "BASE PROMPT" {
			t.Errorf("prompt was modified for a client that renders nothing:\n%s", got)
		}
	}
}
