package storage

import "testing"

// TestCorrectionRecordIsGated pins CorrectionRecord's delegation to
// correction.IsGated (the single socratic pre-reveal predicate, PRD
// §9/§53) — application/anki.Service.GenerateFromCorrection and
// application/lessons.Service.Generate both call this directly (a
// CorrectionRecord is what storage.FeedbackRepository hands them, not a
// feedback.CorrectionView), so it must agree with the HTML/JSON gate
// exactly: HasHint() (non-empty HintJA/HintEN) && Status == "presented"
// && !Revealed.
func TestCorrectionRecordIsGated(t *testing.T) {
	tests := []struct {
		name string
		rec  CorrectionRecord
		want bool
	}{
		{
			name: "hint_presented_unrevealed_is_gated",
			rec:  CorrectionRecord{HintJA: "ヒント", Status: "presented", Revealed: false},
			want: true,
		},
		{
			name: "no_hint_never_gated",
			rec:  CorrectionRecord{Status: "presented", Revealed: false},
			want: false,
		},
		{
			name: "accepted_not_gated_even_with_hint",
			rec:  CorrectionRecord{HintJA: "ヒント", HintEN: "hint", Status: "accepted", Revealed: false},
			want: false,
		},
		{
			name: "revealed_not_gated_even_while_presented",
			rec:  CorrectionRecord{HintJA: "ヒント", Status: "presented", Revealed: true},
			want: false,
		},
		{
			name: "english_only_hint_still_counts_as_hint",
			rec:  CorrectionRecord{HintEN: "hint", Status: "presented", Revealed: false},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.IsGated(); got != tt.want {
				t.Errorf("IsGated() = %v, want %v", got, tt.want)
			}
		})
	}
}
