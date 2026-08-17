package httpx

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
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
// MeaningEN/Tags are Phase 3 Task 8's POST /api/v1/words fields — the
// template shows them only when non-empty (TagsJoined is "" for an
// item with no tags, which {{if}} treats the same as absent), since
// most items still only ever came from a vocabulary.lookup event and
// have neither.
//
// DeleteAction is the row's 削除 button target, built here rather than
// assembled in the template: it carries the current ?filter= through to
// the POST so the redirect afterwards lands the learner back on the tab
// they were reading, not on すべて.
type vocabularyItemView struct {
	Expression, Reading, Meaning, KindLabel, Source                string
	MeaningEN, TagsJoined                                          string
	DeleteAction                                                   string
	Lookups, Productions, SuccessfulProductions, FailedProductions int
}

func toVocabularyItemView(item vocabulary.Item, filter string) vocabularyItemView {
	return vocabularyItemView{
		DeleteAction:          "/vocabulary/" + item.ID + "/delete" + filterQuery(filter),
		Expression:            item.Expression,
		Reading:               item.Reading,
		Meaning:               item.Meaning,
		KindLabel:             vocabularyKindLabel(item.Kind),
		Source:                item.Source,
		MeaningEN:             item.MeaningEN,
		TagsJoined:            strings.Join(item.Tags, ", "),
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
		views = append(views, toVocabularyItemView(item, filter))
	}

	// The undo button carries the tab along, but only when there is
	// something to undo — appending the filter to an empty action would
	// leave a non-empty string and render a banner pointing at nothing.
	restore := undoRestoreAction(r, "/vocabulary")
	if restore != "" {
		restore += filterQuery(filter)
	}

	s.render(w, r, "vocabulary", map[string]any{
		"Title":         "語彙",
		"Identity":      ident,
		"Items":         views,
		"Filter":        filter,
		"RestoreAction": restore,
	})
}

// vocabularyDelete handles POST /vocabulary/{id}/delete: the learner's
// confirmed 削除 of one word. It stops appearing on all four filter
// tabs, in the agent tools, in the activation candidates the teacher
// agent is fed, in the production-detection scan and in the weekly
// summary. Nothing is erased — /learner's vocabulary funnel and
// /outcomes read exactly the same afterwards.
//
// The identity comes from the request context and NOTHING else. The id
// in the path is the only caller-supplied input, and the repository's
// WHERE pairs it with this identity, so another learner's word answers
// 404 exactly like an id that never existed and is left untouched.
func (s *Server) vocabularyDelete(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := s.opts.Vocabulary.Delete(r.Context(), ident.ID, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not delete vocabulary item", http.StatusInternalServerError)
		return
	}
	q := url.Values{"undo": {id}}
	if f := r.URL.Query().Get("filter"); f != "" {
		q.Set("filter", f)
	}
	http.Redirect(w, r, "/vocabulary?"+q.Encode(), http.StatusSeeOther)
}

// vocabularyRestore handles POST /vocabulary/{id}/restore — the undo
// affordance's target. Same identity-from-context rule and same
// 404-for-someone-else's-word contract as vocabularyDelete.
func (s *Server) vocabularyRestore(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	id := chi.URLParam(r, "id")
	if err := s.opts.Vocabulary.Restore(r.Context(), ident.ID, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not restore vocabulary item", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/vocabulary"+filterQuery(r.URL.Query().Get("filter")), http.StatusSeeOther)
}

// filterQuery renders /vocabulary's current tab as a query string
// suffix ("" for すべて), so a delete or an undo returns the learner to
// the tab they were on. Written once here rather than at each of the
// four call sites that need it.
func filterQuery(filter string) string {
	if filter == "" {
		return ""
	}
	return "?" + url.Values{"filter": {filter}}.Encode()
}
