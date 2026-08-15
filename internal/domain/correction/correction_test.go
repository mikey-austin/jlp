package correction

import (
	"errors"
	"testing"
)

func TestNewResult(t *testing.T) {
	tests := []struct {
		name              string
		selection         string
		corrections       []Correction
		wantCorrected     string
		wantAppliedCount  int
		wantError         bool
		wantErrorContains string
	}{
		{
			name:      "prd_example",
			selection: "昨日友達と映画を見に行って、とても面白いでした。",
			corrections: []Correction{
				{
					Original:    "面白いでした",
					Replacement: "面白かったです",
					Type:        "conjugation",
					Severity:    "unnatural",
					Explanation: Explanation{JA: "いでした形は自然ではない", EN: "i-deshita is unnatural"},
					Concepts:    []string{"past-tense"},
				},
			},
			wantCorrected:    "昨日友達と映画を見に行って、とても面白かったです。",
			wantAppliedCount: 1,
			wantError:        false,
		},
		{
			name:      "repeated_substring_first_occurrence",
			selection: "いいですいいです",
			corrections: []Correction{
				{
					Original:    "いいです",
					Replacement: "よかったです",
					Type:        "vocabulary",
					Severity:    "less-natural",
					Explanation: Explanation{JA: "より自然な表現", EN: "more natural"},
				},
			},
			wantCorrected:    "よかったですいいです",
			wantAppliedCount: 1,
			wantError:        false,
		},
		{
			name:      "multiple_corrections_different_parts",
			selection: "これはいいです。あれもよくないです。",
			corrections: []Correction{
				{
					Original:    "いいです",
					Replacement: "良いです",
					Type:        "register",
					Severity:    "style",
					Explanation: Explanation{JA: "より丁寧な表現", EN: "more polite"},
				},
				{
					Original:    "よくないです",
					Replacement: "良くありません",
					Type:        "register",
					Severity:    "style",
					Explanation: Explanation{JA: "より丁寧な表現", EN: "more polite"},
				},
			},
			wantCorrected:    "これは良いです。あれも良くありません。",
			wantAppliedCount: 2,
			wantError:        false,
		},
		{
			name:      "not_found_correction_skipped",
			selection: "これはいいです",
			corrections: []Correction{
				{
					Original:    "存在しないテキスト",
					Replacement: "replacement",
					Type:        "vocabulary",
					Severity:    "style",
					Explanation: Explanation{JA: "ja", EN: "en"},
				},
				{
					Original:    "いいです",
					Replacement: "よろしい",
					Type:        "vocabulary",
					Severity:    "optional",
					Explanation: Explanation{JA: "ja", EN: "en"},
				},
			},
			wantCorrected:    "これはよろしい",
			wantAppliedCount: 1,
			wantError:        false,
		},
		{
			name:      "all_miss_returns_error",
			selection: "これはいいです",
			corrections: []Correction{
				{
					Original:    "存在しないテキスト1",
					Replacement: "replacement",
					Type:        "vocabulary",
					Severity:    "style",
					Explanation: Explanation{JA: "ja", EN: "en"},
				},
				{
					Original:    "存在しないテキスト2",
					Replacement: "replacement",
					Type:        "vocabulary",
					Severity:    "style",
					Explanation: Explanation{JA: "ja", EN: "en"},
				},
			},
			wantCorrected:     "これはいいです",
			wantAppliedCount:  0,
			wantError:         true,
			wantErrorContains: "correction original not found",
		},
		{
			name:             "empty_corrections_list",
			selection:        "これはいいです",
			corrections:      []Correction{},
			wantCorrected:    "これはいいです",
			wantAppliedCount: 0,
			wantError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NewResult(tt.selection, tt.corrections)

			// Check error
			if (err != nil) != tt.wantError {
				t.Errorf("NewResult() error = %v, wantError %v", err, tt.wantError)
			}

			if tt.wantError && err != nil {
				if !errors.Is(err, ErrNoMatch) {
					t.Errorf("NewResult() error = %v, want to wrap ErrNoMatch", err)
				}
			}

			// Check corrected text
			if result.Corrected != tt.wantCorrected {
				t.Errorf("NewResult() Corrected = %q, want %q", result.Corrected, tt.wantCorrected)
			}

			// Check applied count
			if len(result.Corrections) != tt.wantAppliedCount {
				t.Errorf("NewResult() applied count = %d, want %d", len(result.Corrections), tt.wantAppliedCount)
			}

			// Check that all applied corrections have UUIDs
			for i, corr := range result.Corrections {
				if corr.ID == "" {
					t.Errorf("NewResult() Corrections[%d].ID is empty", i)
				}
			}

			// Check original
			if result.Original != tt.selection {
				t.Errorf("NewResult() Original = %q, want %q", result.Original, tt.selection)
			}
		})
	}
}

func TestNewResultIDUniqueness(t *testing.T) {
	selection := "いいですいいです"
	corrections := []Correction{
		{
			Original:    "いいです",
			Replacement: "よかったです",
			Type:        "vocabulary",
			Severity:    "less-natural",
			Explanation: Explanation{JA: "ja", EN: "en"},
		},
		{
			Original:    "いいです",
			Replacement: "よかったです",
			Type:        "vocabulary",
			Severity:    "less-natural",
			Explanation: Explanation{JA: "ja", EN: "en"},
		},
	}

	result, err := NewResult(selection, corrections)
	if err != nil {
		t.Fatalf("NewResult() error = %v, want nil", err)
	}

	if len(result.Corrections) != 2 {
		t.Errorf("NewResult() applied count = %d, want 2", len(result.Corrections))
	}

	if result.Corrections[0].ID == result.Corrections[1].ID {
		t.Errorf("NewResult() generated duplicate IDs: %q", result.Corrections[0].ID)
	}
}

func TestNewResultTwoIdenticalCorrectionsBindLeftToRight(t *testing.T) {
	selection := "いいですいいです"
	corrections := []Correction{
		{
			Original:    "いいです",
			Replacement: "よかったです",
			Type:        "vocabulary",
			Severity:    "less-natural",
			Explanation: Explanation{JA: "ja", EN: "en"},
		},
		{
			Original:    "いいです",
			Replacement: "素晴らしい",
			Type:        "vocabulary",
			Severity:    "style",
			Explanation: Explanation{JA: "ja", EN: "en"},
		},
	}

	result, err := NewResult(selection, corrections)
	if err != nil {
		t.Fatalf("NewResult() error = %v, want nil", err)
	}

	// First "いいです" should be replaced with "よかったです"
	// Second "いいです" should be replaced with "素晴らしい"
	want := "よかったです素晴らしい"
	if result.Corrected != want {
		t.Errorf("NewResult() Corrected = %q, want %q", result.Corrected, want)
	}

	if len(result.Corrections) != 2 {
		t.Errorf("NewResult() applied count = %d, want 2", len(result.Corrections))
	}
}

func TestNewResultCursorAdvancesPastReplacement(t *testing.T) {
	// CRITICAL: cursor must advance past replacement to prevent re-matching inside the replacement
	selection := "いいですいいです"
	corrections := []Correction{
		{
			Original:    "いいです",
			Replacement: "いいですね",
			Type:        "vocabulary",
			Severity:    "optional",
			Explanation: Explanation{JA: "ja", EN: "en"},
		},
		{
			Original:    "いいです",
			Replacement: "よかったです",
			Type:        "vocabulary",
			Severity:    "less-natural",
			Explanation: Explanation{JA: "ja", EN: "en"},
		},
	}

	result, err := NewResult(selection, corrections)
	if err != nil {
		t.Fatalf("NewResult() error = %v, want nil", err)
	}

	// First "いいです" (position 0-3) should be replaced with "いいですね"
	// The second correction must NOT re-match the "いいです" inside "いいですね"
	// Second "いいです" (position 6-9) should be replaced with "よかったです"
	want := "いいですねよかったです"
	if result.Corrected != want {
		t.Errorf("NewResult() Corrected = %q, want %q (cursor must advance past replacement)", result.Corrected, want)
	}

	if len(result.Corrections) != 2 {
		t.Errorf("NewResult() applied count = %d, want 2", len(result.Corrections))
	}
}

// TestIsGated pins the single socratic pre-reveal predicate (PRD
// §9/§53) every layer — the HTML correction_card partial, the JSON API,
// application/anki's GenerateFromCorrection, and application/lessons'
// context formatter — must evaluate identically: a hint-bearing,
// still-"presented", not-yet-revealed correction is gated; anything
// else (no hint, already resolved, or already revealed) is not.
func TestIsGated(t *testing.T) {
	tests := []struct {
		name     string
		hasHint  bool
		status   string
		revealed bool
		want     bool
	}{
		{"hint_presented_unrevealed_is_gated", true, "presented", false, true},
		{"no_hint_never_gated", false, "presented", false, false},
		{"accepted_not_gated_even_with_hint", true, "accepted", false, false},
		{"rejected_not_gated_even_with_hint", true, "rejected", false, false},
		{"revealed_not_gated_even_while_presented", true, "presented", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGated(tt.hasHint, tt.status, tt.revealed); got != tt.want {
				t.Errorf("IsGated(%v, %q, %v) = %v, want %v", tt.hasHint, tt.status, tt.revealed, got, tt.want)
			}
		})
	}
}
