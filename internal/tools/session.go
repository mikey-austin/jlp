package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// sessionView is the compact, model-facing shape every session tool
// below returns — deliberately narrower than domain/session.Session
// (drops IdentityID, which the model already knows implicitly, and
// CreatedAt, which is rarely useful to an agent deciding what to do
// next).
type sessionView struct {
	ID        string             `json:"id"`
	Title     string             `json:"title"`
	Purpose   string             `json:"purpose"`
	Profile   domsession.Profile `json:"profile"`
	UpdatedAt string             `json:"updated_at"`
}

func toSessionView(s domsession.Session) sessionView {
	return sessionView{
		ID:        string(s.ID),
		Title:     s.Title,
		Purpose:   s.Purpose,
		Profile:   s.Profile,
		UpdatedAt: s.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// SessionTools returns the "session" group's tools (PRD §29):
// get_active_session and get_session_context, both delegating to
// application/sessions.Service — no new business logic, just
// projecting session.Session down to sessionView's compact shape.
func SessionTools(svc *sessions.Service) []Tool {
	return []Tool{
		getActiveSessionTool(svc),
		getSessionContextTool(svc),
	}
}

func getActiveSessionTool(svc *sessions.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_active_session",
			Description: "Returns the caller's most recently updated writing session (title, purpose, teaching profile), or {\"active\":false} if they have none yet. Takes no arguments.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, _ json.RawMessage) (string, error) {
			list, err := svc.List(ctx, identity)
			if err != nil {
				return "", fmt.Errorf("get_active_session: %w", err)
			}
			if len(list) == 0 {
				return `{"active":false}`, nil
			}
			// svc.List's underlying repository already orders by
			// updated_at DESC, but that ordering isn't part of
			// storage.SessionRepository's documented contract — sort
			// defensively here so "most recent" is guaranteed by this
			// tool regardless of what the adapter happens to do.
			sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.After(list[j].UpdatedAt) })
			out, err := json.Marshal(toSessionView(list[0]))
			if err != nil {
				return "", fmt.Errorf("get_active_session: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

type sessionContextArgs struct {
	SessionID string `json:"session_id"`
}

func getSessionContextTool(svc *sessions.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_session_context",
			Description: "Returns one writing session's full context (title, purpose, teaching profile) by ID. Use get_active_session first if you don't already have a session_id.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string","description":"The session ID to look up."}},"required":["session_id"],"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[sessionContextArgs](args)
			if err != nil {
				return "", fmt.Errorf("get_session_context: %w", err)
			}
			if a.SessionID == "" {
				return "", fmt.Errorf("get_session_context: session_id is required")
			}
			s, err := svc.Get(ctx, identity, domsession.ID(a.SessionID))
			if err != nil {
				return "", fmt.Errorf("get_session_context: %w", err)
			}
			out, err := json.Marshal(toSessionView(s))
			if err != nil {
				return "", fmt.Errorf("get_session_context: encode: %w", err)
			}
			return string(out), nil
		},
	}
}
