package a2a

import (
	"encoding/json"
	"log/slog"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/domain/diff"
)

// Rich content: turning the structured data an agent read into A2A data
// parts, so a client can render it instead of reading a paragraph about
// it. See docs/superpowers/specs/2026-08-18-a2a-rich-content-design.md.
//
// The prose part is always emitted and always complete on its own. A2A
// clients may ignore parts they do not understand, and other clients
// will — a widget never carries the only copy of something the reader
// needs.

// Media types are JLP's own, named so an unknown one degrades to prose
// in any client rather than breaking it. "+json" so a generic client can
// still parse the value as data.
const (
	mediaCorrection = "application/vnd.jlp.correction+json"
	mediaVocabulary = "application/vnd.jlp.vocabulary+json"
	mediaPriorities = "application/vnd.jlp.priorities+json"
	mediaLesson     = "application/vnd.jlp.lesson+json"
)

// renderableTools maps a tool name to the media type its result renders
// as. A tool absent from this map contributes no data part — the
// default is prose, and adding a widget is an explicit act.
//
// Several tools share a media type on purpose: get_recent_errors and
// get_correction_history both return correction lists, and a client
// should not need to know which one the agent happened to call.
var renderableTools = map[string]string{
	"get_recent_errors":       mediaCorrection,
	"get_correction_history":  mediaCorrection,
	"get_vocabulary_history":  mediaVocabulary,
	"search_vocabulary":       mediaVocabulary,
	"get_learning_priorities": mediaPriorities,
	"create_lesson_plan":      mediaLesson,
}

// widgetParts turns a run's tool calls into data parts.
//
// Deduplicated to the LAST call per media type: an agent that calls
// get_vocabulary_history three times while narrowing down should
// produce the final view, not three stacked widgets. Order follows first
// appearance, so the parts arrive in the order the agent worked.
//
// Nothing here queries storage, and nothing may be added that does. The
// results arrive already redacted by internal/tools (redactIfGated), and
// that is the ONLY reason a widget cannot leak an answer still under the
// socratic gate. Enriching a widget by reading the repository directly —
// to recover, say, the explanation a gated correction is missing — would
// make this the ninth surface that gate has leaked through. See
// TestGatedCorrectionNeverReachesADataPart.
func widgetParts(calls []agentrun.ToolCall) []Part {
	type pending struct {
		media string
		value any
	}
	var order []string
	latest := map[string]pending{}

	for _, call := range calls {
		media, renderable := renderableTools[call.Name]
		if !renderable || call.IsError {
			// A failed tool has an error string where its data should be;
			// rendering that as a widget would present a failure as
			// content. The prose already reports what went wrong.
			continue
		}
		value, ok := decodeWidget(media, call.Result)
		if !ok {
			continue
		}
		if _, seen := latest[media]; !seen {
			order = append(order, media)
		}
		latest[media] = pending{media: media, value: value}
	}

	parts := make([]Part, 0, len(order))
	for _, media := range order {
		p := latest[media]
		parts = append(parts, Part{Data: p.value, MediaType: media})
	}
	return parts
}

// decodeWidget parses a tool's result into the payload a widget renders,
// reporting false for anything that does not parse or that carries
// nothing worth showing.
//
// An empty list returns false deliberately: a widget with no rows is an
// empty box, which reads as "there is nothing" when the truth is "the
// agent looked and this is not what it talked about".
func decodeWidget(media, result string) (any, bool) {
	switch media {
	case mediaCorrection:
		var corrections []correctionPayload
		if err := json.Unmarshal([]byte(result), &corrections); err != nil {
			slog.Warn("a2a: correction tool result did not parse; no widget", "err", err)
			return nil, false
		}
		if len(corrections) == 0 {
			return nil, false
		}
		for i := range corrections {
			corrections[i].addSpans()
		}
		return corrections, true

	case mediaLesson:
		var lesson map[string]any
		if err := json.Unmarshal([]byte(result), &lesson); err != nil {
			slog.Warn("a2a: lesson tool result did not parse; no widget", "err", err)
			return nil, false
		}
		if len(lesson) == 0 {
			return nil, false
		}
		return lesson, true

	default:
		// Vocabulary and priorities are lists this adapter does not need
		// to understand — it passes them through as the tool shaped them.
		var rows []map[string]any
		if err := json.Unmarshal([]byte(result), &rows); err != nil {
			slog.Warn("a2a: tool result did not parse as a list; no widget", "media", media, "err", err)
			return nil, false
		}
		if len(rows) == 0 {
			return nil, false
		}
		return rows, true
	}
}

// correctionPayload is internal/tools' correctionView plus the diff a
// client cannot compute for itself.
//
// The extra field is the one deliberate exception to "a payload is the
// tool's result verbatim": computing the diff in JavaScript would mean a
// second implementation, in another language, that has to agree with
// internal/domain/diff about Japanese — and diff algorithms disagree at
// exactly the boundaries that matter here (okurigana, particles). One
// correction rendering two different ways in two places is the failure
// that avoids.
type correctionPayload struct {
	ID            string `json:"id,omitempty"`
	Original      string `json:"original"`
	Replacement   string `json:"replacement,omitempty"`
	Type          string `json:"type"`
	Severity      string `json:"severity"`
	ExplanationEN string `json:"explanation_en,omitempty"`
	Status        string `json:"status,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	Confidence    *int   `json:"confidence,omitempty"`
	// Spans is the character-level diff from Original to Replacement,
	// absent when there is no replacement to diff against.
	Spans []diffSpan `json:"spans,omitempty"`
}

// diffSpan is one run of text and what happened to it: "equal",
// "insert" or "delete" — the same three the HTML correction card
// renders as plain text, .d-ins and .d-del.
type diffSpan struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// addSpans fills Spans from Original/Replacement.
//
// A gated correction has NO replacement — internal/tools.redactIfGated
// blanked it — so it gets no spans at all. That is not an optimisation:
// diffing against an empty string would mark the entire sentence as
// deleted, which is both wrong and a tell that something was withheld.
// The client renders the gated state from the absence of spans.
func (c *correctionPayload) addSpans() {
	if c.Replacement == "" {
		return
	}
	for _, s := range diff.Runes(c.Original, c.Replacement) {
		c.Spans = append(c.Spans, diffSpan{Op: opName(s.Op), Text: s.Text})
	}
}

// opName spells a diff.Op for the wire. Words rather than the enum's
// integers: a payload crossing a process boundary should not depend on
// the iota order of a Go constant block, which is free to change without
// anyone thinking about JSON.
func opName(op diff.Op) string {
	switch op {
	case diff.OpInsert:
		return "insert"
	case diff.OpDelete:
		return "delete"
	default:
		return "equal"
	}
}
