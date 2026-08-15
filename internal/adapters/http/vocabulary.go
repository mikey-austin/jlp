package httpx

import (
	"net/http"

	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

// vocabularyKindLabels maps vocabulary.Kind to the Japanese label the
// /vocabulary table shows instead of the raw enum value — the same
// role jlptChip plays for grammar.Concept.JLPTLevel on /grammar.
var vocabularyKindLabels = map[vocabulary.Kind]string{
	vocabulary.KindWord:        "単語",
	vocabulary.KindExpression:  "表現",
	vocabulary.KindCollocation: "連語",
	vocabulary.KindPattern:     "パターン",
}

func vocabularyKindLabel(k vocabulary.Kind) string {
	if label, ok := vocabularyKindLabels[k]; ok {
		return label
	}
	return string(k)
}

// vocabularyItemView is one vocabulary.Item as the /vocabulary table
// renders it: FailedProductions is precomputed here (rather than
// subtracted in the template — html/template has no arithmetic, see
// render.go's funcs doc comment) as Productions-SuccessfulProductions,
// the ✗ half of the ✓/✗ production ladder the brief calls for.
type vocabularyItemView struct {
	Expression, Reading, Meaning, KindLabel, Source                string
	Lookups, Productions, SuccessfulProductions, FailedProductions int
}

func toVocabularyItemView(item vocabulary.Item) vocabularyItemView {
	return vocabularyItemView{
		Expression:            item.Expression,
		Reading:               item.Reading,
		Meaning:               item.Meaning,
		KindLabel:             vocabularyKindLabel(item.Kind),
		Source:                item.Source,
		Lookups:               item.Lookups,
		Productions:           item.Productions,
		SuccessfulProductions: item.SuccessfulProductions,
		FailedProductions:     item.Productions - item.SuccessfulProductions,
	}
}

// vocabularyPage handles GET /vocabulary: the identity's personal
// vocabulary catalog (Task 6, PRD §12), narrowed by the ?filter= query
// param — "" (すべて), "looked-up" (調べた), "produced" (使えた), or
// "activate" (活性化候補 — Task 7's expression-bank activation
// candidates; see storage.VocabularyRepository.List's doc comment for
// the exact condition).
func (s *Server) vocabularyPage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	filter := r.URL.Query().Get("filter")

	items, err := s.opts.Vocabulary.List(r.Context(), ident.ID, filter)
	if err != nil {
		http.Error(w, "could not load vocabulary", http.StatusInternalServerError)
		return
	}

	views := make([]vocabularyItemView, 0, len(items))
	for _, item := range items {
		views = append(views, toVocabularyItemView(item))
	}

	Render(w, r, "vocabulary", map[string]any{
		"Title":    "語彙",
		"Identity": ident,
		"Items":    views,
		"Filter":   filter,
	})
}
