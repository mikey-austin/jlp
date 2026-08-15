package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// recentWritingPreviewRunes caps how much document content
// get_recent_writing returns — the model needs enough to see what the
// learner has been writing, not the whole document verbatim (this
// task's design guidance: keep tool outputs small).
const recentWritingPreviewRunes = 1000

// WritingTools returns the "writing" group's tools (PRD §29):
// get_recent_writing, delegating to application/writing.Service.Open.
func WritingTools(svc *appwriting.Service) []Tool {
	return []Tool{getRecentWritingTool(svc)}
}

type recentWritingArgs struct {
	SessionID string `json:"session_id"`
}

type recentWritingView struct {
	DocumentID string `json:"document_id"`
	Version    int    `json:"version"`
	RuneCount  int    `json:"rune_count"`
	Preview    string `json:"preview"`
	Truncated  bool   `json:"truncated"`
}

func getRecentWritingTool(svc *appwriting.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name: "get_recent_writing",
			// Open (application/writing.Service) creates the session's
			// document on first access if it doesn't exist yet — calling
			// this on a session with no writing yet returns an empty
			// preview rather than an error, it does not fabricate content.
			Description: "Returns a preview of the learner's writing for one session (its document content, most recent version): session_id is required. Use get_active_session first if you don't already have one.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string","description":"The session whose document to read."}},"required":["session_id"],"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[recentWritingArgs](args)
			if err != nil {
				return "", fmt.Errorf("get_recent_writing: %w", err)
			}
			if a.SessionID == "" {
				return "", fmt.Errorf("get_recent_writing: session_id is required")
			}
			doc, err := svc.Open(ctx, identity, domsession.ID(a.SessionID))
			if err != nil {
				return "", fmt.Errorf("get_recent_writing: %w", err)
			}

			runes := []rune(doc.Content)
			truncated := len(runes) > recentWritingPreviewRunes
			preview := doc.Content
			if truncated {
				preview = string(runes[:recentWritingPreviewRunes])
			}
			out, err := json.Marshal(recentWritingView{
				DocumentID: string(doc.ID),
				Version:    doc.Version,
				RuneCount:  utf8.RuneCountInString(doc.Content),
				Preview:    preview,
				Truncated:  truncated,
			})
			if err != nil {
				return "", fmt.Errorf("get_recent_writing: encode: %w", err)
			}
			return string(out), nil
		},
	}
}
