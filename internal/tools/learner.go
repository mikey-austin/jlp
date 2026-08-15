package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	learnerProfileMaxObservations = 10
	recentErrorsDefaultLimit      = 5
	recentErrorsMaxLimit          = 20
	correctionHistoryDefaultLimit = 10
	correctionHistoryMaxLimit     = 30
)

// LearnerTools returns the "learner" group's tools (PRD §29):
// get_learner_profile, get_recent_errors, get_correction_history — all
// read-only views over the learner model and feedback history,
// delegating directly to their owning repositories (no application
// service wraps these particular reads, so the repository IS the
// existing thing being delegated to, per the task brief).
func LearnerTools(identities storage.IdentityRepository, obs storage.ObservationRepository, feedback storage.FeedbackRepository) []Tool {
	return []Tool{
		getLearnerProfileTool(identities, obs),
		getRecentErrorsTool(feedback),
		getCorrectionHistoryTool(feedback),
	}
}

type observationView struct {
	Kind        string  `json:"kind"`
	SubjectType string  `json:"subject_type"`
	Subject     string  `json:"subject"`
	Confidence  float64 `json:"confidence"`
}

type learnerProfileView struct {
	DisplayName  string            `json:"display_name"`
	Observations []observationView `json:"observations"`
}

func getLearnerProfileTool(identities storage.IdentityRepository, obs storage.ObservationRepository) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_learner_profile",
			Description: "Returns the learner's display name and their standing learner-model observations (weaknesses/emerging trends), highest confidence first, capped at 10. Takes no arguments.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, _ json.RawMessage) (string, error) {
			id, err := identities.Get(ctx, identity)
			if err != nil {
				return "", fmt.Errorf("get_learner_profile: %w", err)
			}
			observations, err := obs.List(ctx, identity)
			if err != nil {
				return "", fmt.Errorf("get_learner_profile: %w", err)
			}
			sort.Slice(observations, func(i, j int) bool { return observations[i].Confidence > observations[j].Confidence })
			if len(observations) > learnerProfileMaxObservations {
				observations = observations[:learnerProfileMaxObservations]
			}
			views := make([]observationView, 0, len(observations))
			for _, o := range observations {
				views = append(views, observationView{
					Kind:        string(o.Kind),
					SubjectType: string(o.SubjectType),
					Subject:     o.Subject,
					Confidence:  o.Confidence,
				})
			}
			out, err := json.Marshal(learnerProfileView{DisplayName: id.DisplayName, Observations: views})
			if err != nil {
				return "", fmt.Errorf("get_learner_profile: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

// correctionView is the compact, model-facing shape both
// get_recent_errors and get_correction_history return — the same
// underlying storage.CorrectionRecord, projected to two different
// field sets: get_recent_errors focuses on what to teach (the mistake
// and its explanation); get_correction_history additionally surfaces
// how the learner responded (status/attempts/confidence), for an
// agent reasoning about active-recall progress rather than content.
type correctionView struct {
	ID            string `json:"id,omitempty"`
	Original      string `json:"original"`
	Replacement   string `json:"replacement,omitempty"`
	Type          string `json:"type"`
	Severity      string `json:"severity"`
	ExplanationEN string `json:"explanation_en,omitempty"`
	Status        string `json:"status,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	Confidence    *int   `json:"confidence,omitempty"`
}

// redactIfGated blanks the fields that would hand a learner the answer
// to a correction still under PRD §9/§53's socratic active-recall gate
// — storage.CorrectionRecord.IsGated(), the SAME predicate the HTML
// correction_card partial, the JSON API, application/lessons, and
// application/anki all apply before showing a correction to a learner
// (see CorrectionRecord.IsGated's doc comment: "none of them may
// independently redefine 'hidden'"). storage.FeedbackRepository.
// RecentCorrections — what both tools below delegate to — is
// deliberately unfiltered by status (other callers need every
// correction regardless of gate state), so this package is the one
// responsible for withholding the answer here. This is also what the
// MODEL itself sees, not just the learner reading the trace viewer
// afterward: withholding it here is correct either way, since the
// teacher's whole job is to elicit the answer, not hand it out via a
// tool call. Replacement is the answer itself; ExplanationEN typically
// restates the correct form directly ("...so X must be Y"), so it
// leaks the same information and is redacted too.
func redactIfGated(v correctionView, gated bool) correctionView {
	if gated {
		v.Replacement = ""
		v.ExplanationEN = ""
	}
	return v
}

type recentErrorsArgs struct {
	Limit int `json:"limit"`
}

func getRecentErrorsTool(feedback storage.FeedbackRepository) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_recent_errors",
			Description: "Returns the learner's most recent writing corrections (mistake, correction, and why), newest first. Optional limit (default 5, max 20).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[recentErrorsArgs](args)
			if err != nil {
				return "", fmt.Errorf("get_recent_errors: %w", err)
			}
			limit := clampLimit(a.Limit, recentErrorsDefaultLimit, recentErrorsMaxLimit)
			corrections, err := feedback.RecentCorrections(ctx, identity, limit)
			if err != nil {
				return "", fmt.Errorf("get_recent_errors: %w", err)
			}
			views := make([]correctionView, 0, len(corrections))
			for _, c := range corrections {
				views = append(views, redactIfGated(correctionView{
					Original:      c.Original,
					Replacement:   c.Replacement,
					Type:          c.Type,
					Severity:      c.Severity,
					ExplanationEN: c.ExplanationEN,
				}, c.IsGated()))
			}
			out, err := json.Marshal(views)
			if err != nil {
				return "", fmt.Errorf("get_recent_errors: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

type correctionHistoryArgs struct {
	Limit int `json:"limit"`
}

func getCorrectionHistoryTool(feedback storage.FeedbackRepository) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "get_correction_history",
			Description: "Returns the learner's recent corrections with their active-recall status (presented/accepted/rejected, retry attempts, self-rated confidence), newest first. Optional limit (default 10, max 30).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[correctionHistoryArgs](args)
			if err != nil {
				return "", fmt.Errorf("get_correction_history: %w", err)
			}
			limit := clampLimit(a.Limit, correctionHistoryDefaultLimit, correctionHistoryMaxLimit)
			corrections, err := feedback.RecentCorrections(ctx, identity, limit)
			if err != nil {
				return "", fmt.Errorf("get_correction_history: %w", err)
			}
			views := make([]correctionView, 0, len(corrections))
			for _, c := range corrections {
				views = append(views, redactIfGated(correctionView{
					ID:          c.ID,
					Original:    c.Original,
					Replacement: c.Replacement,
					Type:        c.Type,
					Severity:    c.Severity,
					Status:      c.Status,
					Attempts:    c.Attempts,
					Confidence:  c.Confidence,
				}, c.IsGated()))
			}
			out, err := json.Marshal(views)
			if err != nil {
				return "", fmt.Errorf("get_correction_history: encode: %w", err)
			}
			return string(out), nil
		},
	}
}
