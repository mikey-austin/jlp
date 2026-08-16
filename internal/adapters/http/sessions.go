package httpx

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func (s *Server) sessionsList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	list, err := s.opts.Sessions.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	Render(w, r, "sessions", map[string]any{
		"Title":    "セッション",
		"Identity": ident,
		"Sessions": list,
	})
}

func (s *Server) sessionsCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	profile := session.Profile{
		Audience:            r.FormValue("audience"),
		Tone:                r.FormValue("tone"),
		Register:            r.FormValue("register"),
		TeacherMode:         r.FormValue("teacher_mode"),
		ExplanationLanguage: r.FormValue("explanation_language"),
		Strictness:          r.FormValue("strictness"),
		FeedbackTiming:      r.FormValue("feedback_timing"),
	}
	sess, err := s.opts.Sessions.Create(r.Context(), ident.ID, r.FormValue("title"), r.FormValue("purpose"), profile)
	if err != nil {
		if errors.Is(err, sessions.ErrInvalidTitle) {
			http.Error(w, sessions.ErrInvalidTitle.Error(), http.StatusBadRequest)
			return
		}
		// Anything else (a repository failure, most likely) is never
		// echoed back verbatim — it could leak internal detail (a DSN, a
		// driver error, a stack fragment) to the client.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/sessions/"+string(sess.ID), http.StatusSeeOther)
}

func (s *Server) sessionsWorkspace(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := session.ID(chi.URLParam(r, "id"))
	sess, err := s.opts.Sessions.Get(r.Context(), ident.ID, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not load session", http.StatusInternalServerError)
		return
	}
	doc, err := s.opts.Writing.Open(r.Context(), ident.ID, sess.ID)
	if err != nil {
		http.Error(w, "could not load document", http.StatusInternalServerError)
		return
	}

	history, err := s.opts.Conversation.History(r.Context(), ident.ID, sess.ID)
	if err != nil {
		http.Error(w, "could not load conversation", http.StatusInternalServerError)
		return
	}
	turns := make([]conversationTurnView, 0, len(history))
	for _, t := range history {
		turns = append(turns, toConversationTurnView(t))
	}

	Render(w, r, "workspace", map[string]any{
		"Title":              sess.Title,
		"Identity":           ident,
		"Session":            sess,
		"Document":           doc,
		"ConversationTurns":  turns,
		"FeedbackTimingCopy": feedbackTimingCopy(sess.Profile.FeedbackTiming),
	})
}

// feedbackTimingCopy is the UI explainer PRD §17.4 asks for: every
// session's conversation pane says WHY corrections are (or aren't)
// showing up as they type, not just what the current setting's label
// is.
func feedbackTimingCopy(timing string) string {
	switch timing {
	case "immediate":
		return "訂正はすぐに表示されます。"
	case "delayed":
		return "会話が途切れないよう、訂正は数ターンごとにまとめて表示されます。"
	default: // "end"
		return "会話が途切れないよう、訂正は会話の最後にまとめて表示されます（「会話をまとめる」ボタン）。"
	}
}

// sessionsActivity renders the last 10 learning events for the session,
// newest first, as an htmx fragment for the workspace's context pane.
func (s *Server) sessionsActivity(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := session.ID(chi.URLParam(r, "id"))
	events, err := s.opts.Events.ListRecent(r.Context(), ident.ID, &id, 10)
	if err != nil {
		http.Error(w, "could not load activity", http.StatusInternalServerError)
		return
	}
	RenderPartial(w, r, "activity", map[string]any{"Events": events})
}
