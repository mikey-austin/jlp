package reading

// Block is one piece of an article body in reading order: a paragraph,
// or a figure with its annotated caption.
type Block struct {
	Paragraph []Segment
	Figure    *Figure
	Caption   []Segment
}

// Layout interleaves the in-text figures with the annotated paragraphs:
// figures anchored before the first paragraph lead, the rest follow
// their paragraph in ordinal order. Cover-only figures are left out.
// Both /reading/{id} and the EPUB render from it, so they agree.
func Layout(paragraphs []string, figs []Figure, vocab []VocabularyItem) []Block {
	body := Annotate(paragraphs, vocab)
	after := map[int][]Figure{}
	for _, f := range figs {
		if f.InText {
			after[f.AfterParagraph] = append(after[f.AfterParagraph], f)
		}
	}
	var out []Block
	emit := func(i int) {
		for _, f := range after[i] {
			f := f
			b := Block{Figure: &f}
			if f.Caption != "" {
				b.Caption = Annotate([]string{f.Caption}, vocab)[0]
			}
			out = append(out, b)
		}
	}
	emit(-1)
	for i, p := range body {
		out = append(out, Block{Paragraph: p})
		emit(i)
	}
	return out
}
