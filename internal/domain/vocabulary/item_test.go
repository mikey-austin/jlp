package vocabulary_test

import (
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

// A pattern entry is written with 〜 standing for whatever precedes it,
// and is never written that way in a real sentence. Everything that
// compares an expression against text — finding it, blanking it,
// emphasising it — has to compare this form, or every pattern in a
// learner's vocabulary silently fails to match and falls back to a bare
// card. Which is exactly what happened.
func TestMatchableFormStripsThePatternPlaceholder(t *testing.T) {
	for _, tc := range []struct{ name, expression, want string }{
		{"leading wave dash", "〜というわけではない", "というわけではない"},
		{"leading fullwidth tilde", "～ざるを得ない", "ざるを得ない"},
		{"both ends", "〜きらいがある〜", "きらいがある"},
		{"ordinary word untouched", "紛らわしい", "紛らわしい"},
		{"kanji compound untouched", "曖昧", "曖昧"},
		{"surrounding space", "  〜なりに ", "なりに"},
		{"nothing but a placeholder", "〜", ""},
	} {
		if got := vocabulary.MatchableForm(tc.expression); got != tc.want {
			t.Errorf("%s: MatchableForm(%q) = %q, want %q", tc.name, tc.expression, got, tc.want)
		}
	}
}

// A grammar pattern inflects. Demanding its citation form rejects
// perfectly correct examples — which is exactly what happened: a model
// wrote 「というわけでは*ありません*」 for 〜というわけではない and the
// sentence was thrown away.
func TestMatchInFindsAnInflectedPattern(t *testing.T) {
	for _, tc := range []struct {
		name, sentence, expression, want string
		ok                               bool
	}{
		{
			name: "exact", sentence: "この二つは紛らわしいです。", expression: "紛らわしい",
			want: "紛らわしい", ok: true,
		},
		{
			name:     "pattern used with its placeholder filled",
			sentence: "嫌いというわけではないけれど。", expression: "〜というわけではない",
			want: "というわけではない", ok: true,
		},
		{
			name:     "pattern inflected into the polite negative",
			sentence: "お金がないから嫌いというわけではありません。", expression: "〜というわけではない",
			want: "というわけでは", ok: true,
		},
		{
			name:     "word simply absent",
			sentence: "まったく別の文です。", expression: "紛らわしい",
			ok: false,
		},
		{
			name: "no sentence", sentence: "", expression: "紛らわしい", ok: false,
		},
	} {
		got, ok := vocabulary.MatchIn(tc.sentence, tc.expression)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: matched %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Below the threshold a "match" is a common particle string that says
// nothing about this expression — and blanking it would make a cloze
// with no discernible answer.
func TestMatchInRefusesATrivialPrefix(t *testing.T) {
	// The only shared text is "という", three runes.
	if got, ok := vocabulary.MatchIn("彼は学生だという話です。", "〜というわけではない"); ok {
		t.Errorf("matched %q on a trivial prefix", got)
	}
}

// What is returned must be what is actually IN the sentence: blanking or
// bolding anything else marks text that is not there and leaves the real
// occurrence in place, handing over the answer.
func TestMatchInReturnsTextThatIsActuallyPresent(t *testing.T) {
	sentence := "お金がないから嫌いというわけではありません。"
	got, ok := vocabulary.MatchIn(sentence, "〜というわけではない")
	if !ok {
		t.Fatal("no match")
	}
	if !strings.Contains(sentence, got) {
		t.Errorf("returned %q, which is not in the sentence", got)
	}
}
