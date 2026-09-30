package reading

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func para(s string) DraftBlock { return DraftBlock{Kind: BlockParagraph, Text: s} }
func img(cap string) DraftBlock {
	return DraftBlock{Kind: BlockImage, Figure: Figure{Caption: cap, Data: []byte(cap)}}
}

func TestPageBlocksPlacesFiguresAfterTheirParagraph(t *testing.T) {
	png := testPNG(t, 2, 2)
	text := "第一の段落です。\n\n第二の段落です。"
	blocks, rej := PageBlocks("https://x/1", text, []FigureDraft{
		{Caption: "b", AfterParagraph: 0, Data: png},
		{Caption: "a", AfterParagraph: -1, Data: png},
		{Caption: "bad", Data: []byte("nope")},
		{Caption: "c", AfterText: "第二", Data: png},
	})
	if len(rej) != 1 {
		t.Fatalf("rejected = %v", rej)
	}
	var got []string
	for _, b := range blocks {
		if b.PageURL != "https://x/1" || b.Seq != 0 || b.Page != 0 {
			t.Errorf("block = %+v", b)
		}
		if b.Kind == BlockImage {
			got = append(got, "img:"+b.Figure.Caption)
		} else {
			got = append(got, "p:"+string([]rune(b.Text)[:3]))
		}
	}
	if want := "img:a p:第一の img:b p:第二の img:c"; strings.Join(got, " ") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

func TestSummarise(t *testing.T) {
	bs := []DraftBlock{
		{Page: 1, Kind: BlockParagraph, Text: "あいう"},
		{Page: 1, Kind: BlockParagraph, Text: "えお", Excluded: true},
		{Page: 2, Kind: BlockImage},
		{Page: 2, Kind: BlockImage, Excluded: true},
	}
	want := DraftSummary{Pages: 2, Paragraphs: 2, KeptParagraphs: 1, Images: 2, KeptImages: 1, Chars: 3}
	if got := Summarise(bs); got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestSubmissionAnchorsKeptImagesToKeptParagraphs(t *testing.T) {
	long := strings.Repeat("あ", 60)
	ex := para("除外")
	ex.Excluded = true
	exImg := img("skipped")
	exImg.Excluded = true
	d := Submission(DraftMeta{Title: "題", SourceName: "S", SourceURL: "https://x", Author: "A"}, []DraftBlock{
		img("top"), ex, exImg, para(long), img("mid"), para("次"), img("end"),
	})
	if d.Title != "題" || d.SourceName != "S" || d.SourceURL != "https://x" || d.Author != "A" {
		t.Errorf("meta = %+v", d)
	}
	if want := long + "\n\n次"; d.Content != want {
		t.Errorf("content = %q", d.Content)
	}
	if len(d.Figures) != 3 {
		t.Fatalf("figures = %d", len(d.Figures))
	}
	top, mid, end := d.Figures[0], d.Figures[1], d.Figures[2]
	if top.AfterParagraph != -1 || top.AfterText != "" || !top.Lead {
		t.Errorf("top = %+v", top)
	}
	if mid.AfterParagraph != 0 || mid.AfterText != "" || mid.Lead || !mid.InText || mid.Caption != "mid" {
		t.Errorf("mid = %+v", mid)
	}
	if end.AfterText != "" || end.AfterParagraph != 1 {
		t.Errorf("end = %+v", end)
	}
}

func TestSubmitAnchorsByIndexWhenParagraphsRepeatAPrefix(t *testing.T) {
	credit := "写真：共同通信"
	pic := DraftBlock{Kind: BlockImage, Figure: Figure{Caption: "pic", Data: testPNG(t, 2, 2)}}
	meta := DraftMeta{SourceURL: "https://x/1"}
	d := Submission(meta, []DraftBlock{
		para("一ページ目の本文です。"), para(credit),
		para("二ページ目の本文です。"), para(credit + "（提供）"), pic,
	})
	a, err := NewArticle("id", "me", d, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	figs, _ := NewFigures(d.Figures, a.Paragraphs)
	if len(figs) != 1 || figs[0].AfterParagraph != 3 {
		t.Fatalf("figures = %+v", figs)
	}
	// Same text twice: a prefix match would land on the first.
	d = Submission(meta, []DraftBlock{para(credit), para("本文です。"), para(credit), pic})
	a, _ = NewArticle("id", "me", d, 0, time.Now())
	figs, _ = NewFigures(d.Figures, a.Paragraphs)
	if len(figs) != 1 || figs[0].AfterParagraph != 2 {
		t.Fatalf("figures = %+v", figs)
	}
}

func TestClipTitle(t *testing.T) {
	if got := ClipTitle("  a\n b  "); got != "a b" {
		t.Errorf("got %q", got)
	}
	if n := utf8.RuneCountInString(ClipTitle(strings.Repeat("あ", 1000))); n != 300 {
		t.Errorf("runes = %d", n)
	}
}
