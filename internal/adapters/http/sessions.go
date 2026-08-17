package httpx

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// sessionFormValues carries the new-session form's field values back
// into the "sessions" template: the defaults a fresh GET /sessions
// shows (defaultSessionFormValues), or — after a rejected POST — the
// exact values the learner typed, so the reopened modal never loses
// their input (see renderSessionsCreateError). Field names mirror the
// form's own `name=` attributes, not session.Profile's Go names.
type sessionFormValues struct {
	Title               string
	Purpose             string
	TeacherMode         string
	ExplanationLanguage string
	Strictness          string
	FeedbackTiming      string
}

// defaultSessionFormValues is what an empty GET /sessions has always
// rendered: no title, no purpose picked (the select's first option,
// "Blog post", wins by not marking anything selected), and the three
// profile selects pre-set to session.Service.Create's own defaults —
// this is presentation only, Create still applies its own defaults
// independently for any caller (e.g. the JSON API) that skips the form.
func defaultSessionFormValues() sessionFormValues {
	return sessionFormValues{
		ExplanationLanguage: "both",
		Strictness:          "balanced",
		FeedbackTiming:      "end",
	}
}

func (s *Server) sessionsList(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	list, err := s.opts.Sessions.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "sessions", map[string]any{
		"Title":         "セッション",
		"Identity":      ident,
		"Sessions":      list,
		"FormValues":    defaultSessionFormValues(),
		"RestoreAction": undoRestoreAction(r, "/sessions"),
	})
}

// undoRestoreAction turns the ?undo=<id> a delete redirect leaves
// behind into the POST target of the "削除を取り消す" button the list
// then shows, or "" when there is nothing to undo (the partial renders
// nothing for ""). It is shared by /sessions, /vocabulary and /lessons
// so the three pages cannot drift on how undo is spelled.
//
// The id is validated as a UUID before it is used, and never rendered
// as text — only ever as the middle of a path this function builds. A
// caller who hand-crafts ?undo=<anything else> gets an empty string and
// no banner, so the query parameter cannot be used to point the button
// at an arbitrary URL. It is not an authorization check and does not
// need to be: the restore route it builds re-checks the identity from
// the request context, and an id belonging to someone else 404s there.
func undoRestoreAction(r *http.Request, prefix string) string {
	id := r.URL.Query().Get("undo")
	if id == "" {
		return ""
	}
	if _, err := uuid.Parse(id); err != nil {
		return ""
	}
	return prefix + "/" + id + "/restore"
}

func (s *Server) sessionsCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	values := sessionFormValues{
		Title:               r.FormValue("title"),
		Purpose:             r.FormValue("purpose"),
		TeacherMode:         r.FormValue("teacher_mode"),
		ExplanationLanguage: r.FormValue("explanation_language"),
		Strictness:          r.FormValue("strictness"),
		FeedbackTiming:      r.FormValue("feedback_timing"),
	}
	profile := session.Profile{
		Audience:            r.FormValue("audience"),
		Tone:                r.FormValue("tone"),
		Register:            r.FormValue("register"),
		TeacherMode:         values.TeacherMode,
		ExplanationLanguage: values.ExplanationLanguage,
		Strictness:          values.Strictness,
		FeedbackTiming:      values.FeedbackTiming,
	}
	sess, err := s.opts.Sessions.Create(r.Context(), ident.ID, values.Title, values.Purpose, profile)
	if err != nil {
		if errors.Is(err, sessions.ErrInvalidTitle) {
			s.renderSessionsCreateError(w, r, ident, "タイトルを入力してください。", values)
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

// renderSessionsCreateError re-renders /sessions with the modal open
// (data-reopen on the <dialog>, picked up by app.js's showModal() call
// on load), the validation message visible, and every field the
// learner typed preserved — the brief's explicit "a rejected submission
// that reopens empty is strictly worse than the always-visible form it
// replaces" requirement. Mirrors settings.go's
// renderSettingsError/"re-render the SAME page with the error" pattern.
// 400, not 422, to keep TestSessionsCreateEmptyTitleReturnsBadRequest's
// existing contract (this is the only validation failure Create has).
func (s *Server) renderSessionsCreateError(w http.ResponseWriter, r *http.Request, ident learner.Identity, message string, values sessionFormValues) {
	list, err := s.opts.Sessions.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "sessions", map[string]any{
		"Title":      "セッション",
		"Identity":   ident,
		"Sessions":   list,
		"FormValues": values,
		"FormError":  message,
	})
}

// sessionsDelete handles POST /sessions/{id}/delete: the learner's
// confirmed 削除. The session, its document, its feedback history, its
// corrections and its conversation turns all stop appearing anywhere;
// nothing is erased, so /learner and /outcomes are unchanged.
//
// The identity comes from the request context and NOTHING else — not a
// hidden form field, not a query parameter, not a JSON body. The id in
// the path is the only caller-supplied input, and the repository's
// WHERE pairs it with this identity, so another learner's session
// answers 404 exactly like an id that never existed and is left
// untouched (see storage.SessionRepository.SoftDelete).
//
// Plain form POST + 303, matching sessionsCreate and lessonsComplete,
// rather than an htmx swap: the redirect target carries ?undo=<id>,
// which is what renders the "削除を取り消す" affordance on the list.
func (s *Server) sessionsDelete(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := session.ID(chi.URLParam(r, "id"))
	if err := s.opts.Sessions.Delete(r.Context(), ident.ID, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not delete session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/sessions?"+url.Values{"undo": {string(id)}}.Encode(), http.StatusSeeOther)
}

// sessionsRestore handles POST /sessions/{id}/restore — the undo
// affordance's target, and the same route `jlp restore` exercises
// through the application service. Same identity-from-context rule and
// same 404-for-someone-else's-session contract as sessionsDelete.
func (s *Server) sessionsRestore(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := session.ID(chi.URLParam(r, "id"))
	if err := s.opts.Sessions.Restore(r.Context(), ident.ID, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not restore session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/sessions", http.StatusSeeOther)
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

	// FeedbackHistory (Phase 4 Task W item 2) replaces the old アクティビティ
	// activity feed in the context pane — rendered server-side here
	// (rather than an htmx hx-get="…/activity"-style fragment fetched on
	// load) since the data's already a single cheap query away and this
	// avoids a page-load flash of an empty history list. ActiveID is ""
	// on first load: #feedback-results itself starts empty (nothing has
	// been requested yet this page view), so no row is marked shown —
	// see toFeedbackHistoryView's doc comment for how the POST/GET
	// handlers mark one after that.
	feedbackHistory, err := s.opts.Feedback.ListForSession(r.Context(), ident.ID, sess.ID)
	if err != nil {
		http.Error(w, "could not load feedback history", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "workspace", map[string]any{
		"Title":              sess.Title,
		"Identity":           ident,
		"Session":            sess,
		"Document":           doc,
		"ConversationTurns":  turns,
		"FeedbackTimingCopy": feedbackTimingCopy(sess.Profile.FeedbackTiming),
		"FeedbackHistory":    toFeedbackHistoryView(sess.ID, "", feedbackHistory, false),
		"AIProviders":        aiProviderOptions(s.opts.AIProviders, s.opts.AIDefaultProvider),
		"SpeechEnabled":      s.opts.SpeechEnabled,
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
