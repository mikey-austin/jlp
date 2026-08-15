package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
	"github.com/mikeyaustin/jlp/internal/tools"
)

func TestGetLearnerProfileDelegatesAndSortsByConfidence(t *testing.T) {
	identities := newFakeIdentityRepo()
	if err := identities.Upsert(context.Background(), learner.Identity{ID: testIdentity, DisplayName: "美紀"}); err != nil {
		t.Fatal(err)
	}
	obs := newFakeObservationRepo()
	if err := obs.Upsert(context.Background(), learnermodel.Observation{IdentityID: testIdentity, SubjectType: learnermodel.SubjectConcept, Subject: "low", Confidence: 0.2}); err != nil {
		t.Fatal(err)
	}
	if err := obs.Upsert(context.Background(), learnermodel.Observation{IdentityID: testIdentity, SubjectType: learnermodel.SubjectConcept, Subject: "high", Confidence: 0.9}); err != nil {
		t.Fatal(err)
	}
	if err := obs.Upsert(context.Background(), learnermodel.Observation{IdentityID: otherIdentity, SubjectType: learnermodel.SubjectConcept, Subject: "not-yours", Confidence: 1.0}); err != nil {
		t.Fatal(err)
	}

	tool := findTool(t, tools.LearnerTools(identities, obs, newFakeFeedbackRepo()), "get_learner_profile")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}

	var got struct {
		DisplayName  string `json:"display_name"`
		Observations []struct {
			Subject string `json:"subject"`
		} `json:"observations"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got.DisplayName != "美紀" {
		t.Fatalf("display_name = %q, want 美紀", got.DisplayName)
	}
	if len(got.Observations) != 2 {
		t.Fatalf("len(observations) = %d, want 2 (identity-scoped, not-yours excluded): %s", len(got.Observations), out)
	}
	if got.Observations[0].Subject != "high" {
		t.Fatalf("Observations[0].Subject = %q, want %q (highest confidence first)", got.Observations[0].Subject, "high")
	}
}

func TestGetRecentErrorsDelegatesAndProjectsCompactShape(t *testing.T) {
	feedback := newFakeFeedbackRepo()
	feedback.seed(testIdentity, storage.CorrectionRecord{ID: "c-1", Original: "面白いでした", Replacement: "面白かったです", Type: "conjugation", Severity: "incorrect", ExplanationEN: "i-adjective past tense"})
	feedback.seed(otherIdentity, storage.CorrectionRecord{ID: "c-2", Original: "not-yours"})

	tool := findTool(t, tools.LearnerTools(newFakeIdentityRepo(), newFakeObservationRepo(), feedback), "get_recent_errors")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 || got[0]["original"] != "面白いでした" {
		t.Fatalf("got %v, want exactly testIdentity's one correction: output=%s", got, out)
	}
	// get_recent_errors deliberately omits status/attempts/confidence —
	// that's get_correction_history's job (see correctionView's doc
	// comment) — so a bare correction has neither field present.
	if _, has := got[0]["status"]; has {
		t.Fatalf("output includes status, want it omitted from get_recent_errors: %s", out)
	}
}

// TestGetRecentErrorsRedactsGatedCorrections pins the fix for a
// socratic answer leak found in review: a correction still under PRD
// §9/§53's active-recall gate (hint present, status "presented", not
// revealed — storage.CorrectionRecord.IsGated()) must never hand its
// Replacement or ExplanationEN to the model via this tool, the same
// way every other learner-facing consumer withholds it.
func TestGetRecentErrorsRedactsGatedCorrections(t *testing.T) {
	feedback := newFakeFeedbackRepo()
	feedback.seed(testIdentity, storage.CorrectionRecord{
		ID: "c-gated", Original: "面白いでした", Replacement: "面白かったです",
		Type: "conjugation", Severity: "incorrect",
		ExplanationEN: "i-adjectives form the past tense with 〜かった, so 面白いでした must be 面白かったです.",
		HintJA:        "hint", Status: "presented", Revealed: false,
	})

	tool := findTool(t, tools.LearnerTools(newFakeIdentityRepo(), newFakeObservationRepo(), feedback), "get_recent_errors")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if r, has := got[0]["replacement"]; has {
		t.Fatalf("output includes replacement %q for a gated correction, want it withheld: %s", r, out)
	}
	if e, has := got[0]["explanation_en"]; has {
		t.Fatalf("output includes explanation_en %q for a gated correction (it restates the answer), want it withheld: %s", e, out)
	}
	// The mistake itself (what NOT to do) is still useful context and
	// isn't the withheld answer — it stays.
	if got[0]["original"] != "面白いでした" {
		t.Fatalf("output missing original for a gated correction, want it still present: %s", out)
	}
}

// TestGetCorrectionHistoryRedactsGatedCorrections mirrors the above
// for get_correction_history: active-recall status/attempts/confidence
// stay visible (that's the whole point of this tool), but the answer
// itself must not.
func TestGetCorrectionHistoryRedactsGatedCorrections(t *testing.T) {
	feedback := newFakeFeedbackRepo()
	feedback.seed(testIdentity, storage.CorrectionRecord{
		ID: "c-gated", Original: "x", Replacement: "y",
		HintJA: "hint", Status: "presented", Revealed: false, Attempts: 1,
	})

	tool := findTool(t, tools.LearnerTools(newFakeIdentityRepo(), newFakeObservationRepo(), feedback), "get_correction_history")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if r, has := got[0]["replacement"]; has {
		t.Fatalf("output includes replacement %q for a gated correction, want it withheld: %s", r, out)
	}
	if got[0]["status"] != "presented" || got[0]["attempts"].(float64) != 1 {
		t.Fatalf("output missing status/attempts for a gated correction, want active-recall progress still visible: %s", out)
	}
}

// TestGetCorrectionHistoryKeepsReplacementWhenRevealed pins the other
// side of the gate: once a correction has been revealed (答えを見る) or
// resolved (accepted/rejected), IsGated() is false and the answer is
// no longer withheld — this tool must not over-redact.
func TestGetCorrectionHistoryKeepsReplacementWhenRevealed(t *testing.T) {
	feedback := newFakeFeedbackRepo()
	feedback.seed(testIdentity, storage.CorrectionRecord{
		ID: "c-revealed", Original: "x", Replacement: "y",
		HintJA: "hint", Status: "presented", Revealed: true,
	})

	tool := findTool(t, tools.LearnerTools(newFakeIdentityRepo(), newFakeObservationRepo(), feedback), "get_correction_history")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 || got[0]["replacement"] != "y" {
		t.Fatalf("output = %v, want replacement=y once revealed (not gated anymore): %s", got, out)
	}
}

func TestGetCorrectionHistoryIncludesStatusAndAttempts(t *testing.T) {
	confidence := 4
	feedback := newFakeFeedbackRepo()
	feedback.seed(testIdentity, storage.CorrectionRecord{ID: "c-1", Original: "x", Replacement: "y", Status: "accepted", Attempts: 2, Confidence: &confidence})

	tool := findTool(t, tools.LearnerTools(newFakeIdentityRepo(), newFakeObservationRepo(), feedback), "get_correction_history")
	out, err := tool.Handler(context.Background(), testIdentity, nil, nil)
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0]["status"] != "accepted" || got[0]["attempts"].(float64) != 2 || got[0]["confidence"].(float64) != 4 {
		t.Fatalf("got %v, want status=accepted attempts=2 confidence=4: output=%s", got[0], out)
	}
}
