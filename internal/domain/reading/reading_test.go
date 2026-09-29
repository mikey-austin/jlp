package reading

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

const sampleJA = "政府は２９日、新たな経済対策を発表した。\n\n中央銀行は金融引き締めを続けている。"

func TestNewArticleNormalisesAndHashes(t *testing.T) {
	a, err := NewArticle("a1", "me", Draft{
		SourceURL: "https://jp.wsj.com/articles/abc#comments",
		Title:     "  経済対策\n の行方  ",
		Content:   "\r\n　政府は２９日、新たな\n経済対策を発表した。\r\n\r\n\n中央銀行は金融引き締めを続けている。  \n",
	}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(a.Paragraphs), 2; got != want {
		t.Fatalf("paragraphs = %d (%q), want %d", got, a.Paragraphs, want)
	}
	// Japanese lines inside one paragraph join with no space.
	if a.Paragraphs[0] != "政府は２９日、新たな経済対策を発表した。" {
		t.Fatalf("paragraph 0 = %q", a.Paragraphs[0])
	}
	if a.SourceURL != "https://jp.wsj.com/articles/abc" {
		t.Fatalf("fragment not stripped: %q", a.SourceURL)
	}
	if a.Title != "経済対策 の行方" {
		t.Fatalf("title = %q", a.Title)
	}
	b, err := NewArticle("a2", "me", Draft{Content: sampleJA}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentHash != b.ContentHash {
		t.Fatal("same normalised body must hash the same regardless of whitespace and metadata")
	}
}

func TestNewArticleJoinsWrappedEnglishWithSpace(t *testing.T) {
	got := NormaliseParagraphs("日本語の文章です。Federal\nReserve が利上げ。")
	if len(got) != 1 || got[0] != "日本語の文章です。Federal Reserve が利上げ。" {
		t.Fatalf("got %q", got)
	}
}

func TestNewArticleRejects(t *testing.T) {
	cases := []struct {
		name string
		d    Draft
		max  int
		want error
	}{
		{"empty", Draft{Content: " \n　\n"}, 0, ErrEmptyContent},
		{"english", Draft{Content: "The Federal Reserve raised rates again on Wednesday."}, 0, ErrNotJapanese},
		{"too long", Draft{Content: strings.Repeat("あ", 101)}, 100, ErrArticleTooLarge},
		{"bad scheme", Draft{Content: sampleJA, SourceURL: "javascript:alert(1)"}, 0, ErrInvalidURL},
		{"relative url", Draft{Content: sampleJA, SourceURL: "/articles/1"}, 0, ErrInvalidURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewArticle("x", "me", c.d, c.max, now)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestNewArticleTitleFallsBackToFirstParagraph(t *testing.T) {
	a, err := NewArticle("x", "me", Draft{Content: sampleJA}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Title, "政府は") {
		t.Fatalf("title = %q", a.Title)
	}
}

func TestLessonNormalise(t *testing.T) {
	l := Lesson{
		Vocabulary: []VocabularyItem{
			{Expression: " 金融引き締め ", Reading: "きんゆうひきしめ", MeaningEN: "monetary tightening"},
			{Expression: "金融引き締め", Reading: "x", MeaningEN: "duplicate"},
			{Expression: "", MeaningEN: "no headword"},
			{Expression: "めぐる", Reading: "めぐる", MeaningEN: "to concern"},
		},
		Grammar: []GrammarPoint{{Pattern: ""}, {Pattern: "〜をめぐり"}},
		SentenceAnalyses: []SentenceAnalysis{
			{Sentence: "文。", Chunks: []Chunk{{Text: " "}, {Text: "文", Reading: "ぶん"}, {Text: "です", Reading: "です"}}},
			{Sentence: " "},
		},
		Review: Review{Comprehension: []QA{{QuestionJA: ""}, {QuestionJA: "なぜ？", AnswerJA: "から"}}},
	}
	if err := l.Normalise(); err != nil {
		t.Fatal(err)
	}
	if len(l.Vocabulary) != 2 || l.Vocabulary[0].Expression != "金融引き締め" || l.Vocabulary[0].MeaningEN != "monetary tightening" {
		t.Fatalf("vocabulary = %+v", l.Vocabulary)
	}
	if l.Vocabulary[1].Reading != "" {
		t.Fatalf("kana reading identical to headword should be dropped: %+v", l.Vocabulary[1])
	}
	if len(l.Grammar) != 1 || len(l.SentenceAnalyses) != 1 || len(l.SentenceAnalyses[0].Chunks) != 2 {
		t.Fatalf("grammar/sentences not cleaned: %+v %+v", l.Grammar, l.SentenceAnalyses)
	}
	if l.SentenceAnalyses[0].Chunks[1].Reading != "" {
		t.Fatal("chunk reading identical to text should be dropped")
	}
	if len(l.Review.Comprehension) != 1 {
		t.Fatalf("review = %+v", l.Review)
	}
}

func TestLessonNormaliseRejectsEmptyVocabulary(t *testing.T) {
	l := Lesson{Vocabulary: []VocabularyItem{{Expression: "語", MeaningEN: ""}}}
	if err := l.Normalise(); !errors.Is(err, ErrEmptyLesson) {
		t.Fatalf("err = %v, want ErrEmptyLesson", err)
	}
}

func TestAnnotateFirstOccurrenceLongestMatch(t *testing.T) {
	paras := []string{"金融政策と金融引き締め。", "金融引き締めが続く。"}
	vocab := []VocabularyItem{
		{Expression: "金融", Reading: "きんゆう"},
		{Expression: "金融引き締め", Reading: "きんゆうひきしめ"},
		{Expression: "続く", Reading: ""},       // no reading: skipped
		{Expression: "ひきしめ", Reading: "ひきしめ"}, // no kanji: skipped
	}
	got := Annotate(paras, vocab)
	if len(got) != 2 {
		t.Fatalf("paragraphs = %d", len(got))
	}
	// 金融 first appears standalone in 金融政策; 金融引き締め is its own term.
	want0 := []Segment{
		{Text: "金融", Reading: "きんゆう", Vocab: true},
		{Text: "政策と"},
		{Text: "金融引き締め", Reading: "きんゆうひきしめ", Vocab: true},
		{Text: "。"},
	}
	if !equalSegs(got[0], want0) {
		t.Fatalf("para 0 = %+v", got[0])
	}
	// Second occurrence stays plain.
	if len(got[1]) != 1 || got[1][0].Reading != "" || got[1][0].Text != paras[1] {
		t.Fatalf("para 1 = %+v", got[1])
	}
	// Reassembled text is unchanged.
	for i, segs := range got {
		var b strings.Builder
		for _, s := range segs {
			b.WriteString(s.Text)
		}
		if b.String() != paras[i] {
			t.Fatalf("paragraph %d altered: %q", i, b.String())
		}
	}
}

func equalSegs(a, b []Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRetryDelayGrows(t *testing.T) {
	if RetryDelay(1) >= RetryDelay(2) || RetryDelay(2) >= RetryDelay(3) {
		t.Fatal("backoff must grow")
	}
}
