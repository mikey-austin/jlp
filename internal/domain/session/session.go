package session

import (
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type ID string

type Session struct {
	ID         ID
	IdentityID learner.IdentityID
	Title      string
	Purpose    string
	Profile    Profile
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Profile struct {
	Audience            string
	Tone                string
	Register            string // "casual" | "polite" | "formal" | ""
	TeacherMode         string // "teacher" | "strict-corrector" | "naturalness-coach" | "socratic"
	ExplanationLanguage string // "ja" | "en" | "both"
	Strictness          string // "lenient" | "balanced" | "strict"
	// FeedbackTiming governs when the conversation tutor (Phase 4 Task
	// 6, PRD §17.4) surfaces corrections it finds in the learner's
	// messages: "immediate" shows them inline with each reply,
	// "delayed" withholds them for a few turns and then shows a batch,
	// "end" (the default for every new session — see
	// application/sessions.Service.Create) shows nothing until the
	// conversation is explicitly summarised. PRD §17.4 is explicit that
	// dialogue shouldn't be constantly interrupted by correction UI, so
	// "end" — not "immediate" — is what a learner gets unless they
	// opt into faster feedback themselves.
	FeedbackTiming string // "immediate" | "delayed" | "end"
}
