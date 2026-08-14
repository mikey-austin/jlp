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
}
