package reading

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"
)

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var paras = []string{"第一段落です。", "第二段落です。", "第三段落です。"}

func TestNewFiguresSniffsTypeAndMeasures(t *testing.T) {
	var g bytes.Buffer
	_ = gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 300, 200), []color.Color{color.Black, color.White}), nil)
	figs, rejected := NewFigures([]FigureDraft{
		{Data: testJPEG(t, 640, 480), AfterParagraph: 0, InText: true},
		{Data: testPNG(t, 300, 300), AfterParagraph: 1, InText: true},
		{Data: g.Bytes(), AfterParagraph: 2, InText: true},
	}, paras)
	if len(rejected) != 0 {
		t.Fatalf("rejected: %v", rejected)
	}
	want := []string{"image/jpeg", "image/png", "image/gif"}
	for i, f := range figs {
		if f.MediaType != want[i] || f.Ordinal != i || f.SHA256 == "" {
			t.Fatalf("fig %d = %+v", i, f)
		}
	}
	if figs[0].Width != 640 || figs[0].Height != 480 {
		t.Fatalf("dimensions = %dx%d", figs[0].Width, figs[0].Height)
	}
}

func TestNewFiguresRejectsBadImagesIndividually(t *testing.T) {
	huge := make([]byte, MaxFigureBytes+1)
	copy(huge, testJPEG(t, 10, 10))
	// A PNG header claiming 6000×6000 = 36 MP, over the pixel cap; the
	// body is never decoded, so it need not be valid.
	bomb := testPNG(t, 1, 1)
	bomb[16], bomb[17], bomb[18], bomb[19] = 0, 0, 0x17, 0x70 // width 6000
	bomb[20], bomb[21], bomb[22], bomb[23] = 0, 0, 0x17, 0x70 // height 6000
	// Recompute the CRC over the modified IHDR chunk
	crc := crc32.ChecksumIEEE(bomb[12:29])
	binary.BigEndian.PutUint32(bomb[29:33], crc)

	figs, rejected := NewFigures([]FigureDraft{
		{Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
		{Data: huge},
		{Data: bomb},
		{Data: append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{0}, 64)...)}, // JPEG magic, no image
		{Data: testJPEG(t, 400, 300), InText: true},
	}, paras)
	if len(figs) != 1 || figs[0].Ordinal != 0 {
		t.Fatalf("kept = %+v; the good one must survive, renumbered from 0", figs)
	}
	wantErrs := []error{ErrFigureType, ErrFigureSize, ErrFigurePixels, ErrFigureDecode}
	if len(rejected) != len(wantErrs) {
		t.Fatalf("rejected = %v", rejected)
	}
	for i, e := range wantErrs {
		if !errors.Is(rejected[i], e) {
			t.Fatalf("rejected[%d] = %v, want %v", i, rejected[i], e)
		}
	}
}

func TestNewFiguresIgnoresClaimedTypeAndCapsCountAndLead(t *testing.T) {
	var drafts []FigureDraft
	for i := 0; i < MaxFigures+3; i++ {
		drafts = append(drafts, FigureDraft{Data: testJPEG(t, 300, 300), Lead: true, InText: true,
			Caption: "  " + strings.Repeat("字", 600) + "  ", Alt: strings.Repeat("a", 400)})
	}
	figs, _ := NewFigures(drafts, paras)
	if len(figs) != MaxFigures {
		t.Fatalf("kept %d, want %d", len(figs), MaxFigures)
	}
	leads := 0
	for _, f := range figs {
		if f.Lead {
			leads++
		}
	}
	if leads != 1 || !figs[0].Lead {
		t.Fatalf("leads = %d (first lead wins)", leads)
	}
	if n := len([]rune(figs[0].Caption)); n != 500 {
		t.Fatalf("caption runes = %d", n)
	}
	if n := len([]rune(figs[0].Alt)); n != 300 {
		t.Fatalf("alt runes = %d", n)
	}
}

// The client counts its own blocks; the server may split one of them in
// two (a <p> holding a blank line). after_text anchors the figure to the
// paragraph it really followed; the index is only a fallback.
func TestNewFiguresAnchorsByTextThenClampsIndex(t *testing.T) {
	server := []string{"見出し", "前半です。", "後半です。", "結びです。"}
	figs, _ := NewFigures([]FigureDraft{
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: 1, AfterText: "後半です"},
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: 99},
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: -7},
		{Data: testJPEG(t, 300, 300), InText: true, AfterParagraph: 0, AfterText: "どこにもない"},
	}, server)
	got := []int{figs[0].AfterParagraph, figs[1].AfterParagraph, figs[2].AfterParagraph, figs[3].AfterParagraph}
	want := []int{2, 3, -1, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("positions = %v, want %v", got, want)
		}
	}
}

func TestContentHashIgnoresFigures(t *testing.T) {
	now := time.Unix(0, 0)
	d := Draft{Title: "t", Content: "本文です。日本語の記事。"}
	a1, err := NewArticle("a", "me", d, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	d.Figures = []FigureDraft{{Data: testJPEG(t, 300, 300)}}
	a2, _ := NewArticle("b", "me", d, 0, now)
	if a1.ContentHash != a2.ContentHash {
		t.Fatal("figures must not change the content hash (dedupe)")
	}
}

func TestLayoutInterleavesInTextFiguresOnly(t *testing.T) {
	figs := []Figure{
		{Ordinal: 0, AfterParagraph: -1, InText: true, Caption: "冒頭の写真"},
		{Ordinal: 1, AfterParagraph: 1, InText: true, Caption: "金融政策の図"},
		{Ordinal: 2, AfterParagraph: 1, InText: true},
		{Ordinal: 3, AfterParagraph: 0, InText: false, Lead: true}, // og:image: cover only
	}
	vocab := []VocabularyItem{{Expression: "金融", Reading: "きんゆう", MeaningEN: "finance"}}
	blocks := Layout(paras, figs, vocab)
	var shape []string
	for _, b := range blocks {
		if b.Figure != nil {
			shape = append(shape, "F"+string(rune('0'+b.Figure.Ordinal)))
		} else {
			shape = append(shape, "P")
		}
	}
	if got := strings.Join(shape, " "); got != "F0 P P F1 F2 P" {
		t.Fatalf("layout = %s", got)
	}
	var ruby bool
	for _, s := range blocks[3].Caption {
		if s.Vocab && s.Reading == "きんゆう" {
			ruby = true
		}
	}
	if !ruby {
		t.Fatalf("caption not annotated: %+v", blocks[3].Caption)
	}
}
