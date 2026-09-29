package reading

import (
	"sort"
	"strings"
	"unicode"
)

// Segment is one run of article text, optionally carrying a reading to
// render above it as furigana (<ruby>).
type Segment struct {
	Text    string
	Reading string
	// Vocab marks a run that is one of the edition's 重要語彙, so a
	// renderer can emphasise it or link it to its entry.
	Vocab bool
}

// Annotate splits every paragraph into segments, attaching the reading
// of each 重要語彙 expression to its FIRST occurrence in the article —
// the one place a reader meets it cold. Later occurrences stay plain:
// furigana on every repetition trains you to read the ruby instead of
// the kanji.
//
// Matching is exact and longest-first (金融引き締め wins over 金融 when
// both are listed), and only expressions containing kanji and carrying a
// reading are annotated — ruby over kana is noise. The result has one
// []Segment per paragraph, in order.
func Annotate(paragraphs []string, vocab []VocabularyItem) [][]Segment {
	type term struct{ expr, reading string }
	var terms []term
	for _, v := range vocab {
		if v.Reading == "" || !hasKanji(v.Expression) {
			continue
		}
		terms = append(terms, term{v.Expression, v.Reading})
	}
	sort.SliceStable(terms, func(i, j int) bool { return len(terms[i].expr) > len(terms[j].expr) })

	used := map[string]bool{}
	out := make([][]Segment, 0, len(paragraphs))
	for _, p := range paragraphs {
		var segs []Segment
		rest := p
		for rest != "" {
			// Earliest match in rest; among matches at the same position,
			// the longest (terms is sorted longest-first, so the first
			// hit at a position wins).
			bestAt, bestIdx := -1, -1
			for i, t := range terms {
				if used[t.expr] {
					continue
				}
				at := strings.Index(rest, t.expr)
				if at < 0 {
					continue
				}
				if bestAt == -1 || at < bestAt {
					bestAt, bestIdx = at, i
				}
			}
			if bestIdx == -1 {
				segs = append(segs, Segment{Text: rest})
				break
			}
			t := terms[bestIdx]
			if bestAt > 0 {
				segs = append(segs, Segment{Text: rest[:bestAt]})
			}
			segs = append(segs, Segment{Text: t.expr, Reading: t.reading, Vocab: true})
			used[t.expr] = true
			rest = rest[bestAt+len(t.expr):]
		}
		out = append(out, segs)
	}
	return out
}

func hasKanji(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
