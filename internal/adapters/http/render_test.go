package httpx

import "testing"

// The synthesizer and the browser fallback both speak Japanese. Handed
// English they read it in a Japanese voice, which for a learner is worse
// than silence — it teaches a pronunciation to unlearn. A definition may
// be either language, so the button is decided per string.
func TestHasJapaneseDecidesWhatCanBeSpoken(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"kanji", "面白い", true},
		{"hiragana", "おもしろい", true},
		{"katakana", "コンビニ", true},
		{"english gloss", "confusing, easily mixed up", false},
		{"empty", "", false},
		{"digits and punctuation only", "1, 2, 3.", false},
		{"mixed sentence", "この本はinterestingです", true},
	} {
		if got := hasJapanese(tc.text); got != tc.want {
			t.Errorf("%s: hasJapanese(%q) = %v, want %v", tc.name, tc.text, got, tc.want)
		}
	}
}
