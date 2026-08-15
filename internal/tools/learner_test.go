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
