package epub_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/epub"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

func book() publishing.Ebook {
	pub := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	return publishing.Ebook{
		ID: "6f1f7f9e-7c51-4c1e-9a55-0b5d2c7e3a11",
		Article: reading.Article{
			Title:       "政府、新たな経済対策 <速報> & 解説",
			SourceName:  "WSJ日本版",
			SourceURL:   "https://jp.wsj.com/articles/abc?x=1&y=2",
			Author:      "山田太郎",
			PublishedAt: &pub,
			Paragraphs: []string{
				"政府が発表した新たな経済対策をめぐり、議論が続いている。",
				"中央銀行は金融引き締めを続けている。金融引き締めの影響は大きい。",
				"市場は慎重な見方を崩していない。",
			},
		},
		Lesson: reading.Lesson{
			SummaryEN: "The government announced a package.",
			SummaryJA: "政府が経済対策を発表した。",
			Level:     "N1",
			Vocabulary: []reading.VocabularyItem{
				{Expression: "金融引き締め", Reading: "きんゆうひきしめ", MeaningEN: "monetary tightening", UsageEN: "Central-bank reporting.", ExampleJA: "金融引き締めが続く。", ExampleEN: "Tightening continues."},
				{Expression: "経済対策", Reading: "けいざいたいさく", MeaningEN: "economic package", ExampleJA: "経済対策を打ち出す。"},
			},
			Grammar: []reading.GrammarPoint{{Pattern: "〜をめぐり", MeaningEN: "over", ExplanationEN: "Topic of dispute.", FromArticle: "経済対策をめぐり", ExampleJA: "法案をめぐり対立。", ExampleEN: "Clash over a bill."}},
			SentenceAnalyses: []reading.SentenceAnalysis{{
				Sentence:      "政府が発表した新たな経済対策をめぐり、議論が続いている。",
				TranslationEN: "Debate continues over the new package.",
				Chunks: []reading.Chunk{
					{Text: "政府が発表した", Reading: "せいふがはっぴょうした", RoleEN: "relative clause"},
					{Text: "議論が続いている。", RoleEN: "main clause"},
				},
				NoteEN: "Find the head noun.",
			}},
			Review: reading.Review{
				Comprehension: []reading.QA{{QuestionJA: "何が発表されましたか。", AnswerJA: "経済対策。"}},
				Vocabulary:    []reading.QA{{QuestionJA: "「金融引き締め」の意味は？", AnswerJA: "monetary tightening"}},
			},
		},
		GeneratedAt: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC),
	}
}

func unzip(t *testing.T, data []byte) (map[string]string, []string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	var order []string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		files[f.Name] = string(b)
		order = append(order, f.Name)
		if f.Name == "mimetype" && f.Method != zip.Store {
			t.Fatal("mimetype must be stored uncompressed")
		}
	}
	return files, order
}

func TestRenderStructure(t *testing.T) {
	data, err := epub.New().Render(context.Background(), book())
	if err != nil {
		t.Fatal(err)
	}
	files, order := unzip(t, data)
	if order[0] != "mimetype" || files["mimetype"] != "application/epub+zip" {
		t.Fatalf("first entry = %q %q", order[0], files["mimetype"])
	}
	for _, name := range []string{"META-INF/container.xml", "OEBPS/content.opf", "OEBPS/nav.xhtml", "OEBPS/toc.ncx", "OEBPS/style.css",
		"OEBPS/article.xhtml", "OEBPS/vocabulary.xhtml", "OEBPS/grammar.xhtml", "OEBPS/close-reading.xhtml", "OEBPS/review.xhtml", "OEBPS/answers.xhtml"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("missing %s (have %v)", name, order)
		}
	}
	// Every XML document is well-formed (an e-reader rejects the whole
	// book over one unescaped ampersand).
	for name, body := range files {
		if !strings.HasSuffix(name, ".xhtml") && !strings.HasSuffix(name, ".opf") && !strings.HasSuffix(name, ".ncx") && !strings.HasSuffix(name, ".xml") {
			continue
		}
		dec := xml.NewDecoder(strings.NewReader(body))
		dec.Strict = true
		for {
			_, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s is not well-formed XML: %v\n%s", name, err, body)
			}
		}
	}

	art := files["OEBPS/article.xhtml"]
	// Title escaped, furigana on the FIRST 金融引き締め only.
	if !strings.Contains(art, "政府、新たな経済対策 &lt;速報&gt; &amp; 解説") {
		t.Fatalf("title not escaped:\n%s", art)
	}
	if strings.Count(art, "<rt>きんゆうひきしめ</rt>") != 1 || strings.Count(art, "金融引き締め") != 2 {
		t.Fatalf("furigana should annotate the first occurrence only:\n%s", art)
	}
	if !strings.Contains(art, `lang="ja"`) || !strings.Contains(art, "2026年9月29日") || !strings.Contains(art, "WSJ日本版") {
		t.Fatalf("article metadata missing:\n%s", art)
	}
	// Answers are not on the question page.
	if strings.Contains(files["OEBPS/review.xhtml"], "経済対策。") || !strings.Contains(files["OEBPS/answers.xhtml"], "経済対策。") {
		t.Fatal("answers must be on their own page")
	}
	opf := files["OEBPS/content.opf"]
	for _, want := range []string{`<dc:identifier id="uid">urn:uuid:6f1f7f9e-7c51-4c1e-9a55-0b5d2c7e3a11</dc:identifier>`, "<dc:language>ja</dc:language>", `properties="nav"`, `<itemref idref="answers"/>`, "2026-09-29T10:00:00Z"} {
		if !strings.Contains(opf, want) {
			t.Fatalf("opf missing %q:\n%s", want, opf)
		}
	}
	if !strings.Contains(files["OEBPS/close-reading.xhtml"], "<ruby>政府が発表した<rt>せいふがはっぴょうした</rt></ruby>") {
		t.Fatalf("chunk furigana missing:\n%s", files["OEBPS/close-reading.xhtml"])
	}
}

func TestRenderOmitsEmptySections(t *testing.T) {
	b := book()
	b.Lesson.Grammar, b.Lesson.SentenceAnalyses, b.Lesson.Review = nil, nil, reading.Review{}
	data, err := epub.New().Render(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := unzip(t, data)
	for _, gone := range []string{"OEBPS/grammar.xhtml", "OEBPS/close-reading.xhtml", "OEBPS/review.xhtml", "OEBPS/answers.xhtml"} {
		if _, ok := files[gone]; ok {
			t.Fatalf("%s should be omitted when empty", gone)
		}
	}
	if strings.Contains(files["OEBPS/nav.xhtml"], "精読") {
		t.Fatal("nav lists an omitted chapter")
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	a, _ := epub.New().Render(context.Background(), book())
	b, _ := epub.New().Render(context.Background(), book())
	if !bytes.Equal(a, b) {
		t.Fatal("the same edition must render to the same bytes")
	}
}

// TestEPUBCheck runs the W3C validator when EPUBCHECK_JAR points at
// epubcheck.jar (it is not vendored; CI or a developer opts in).
func TestEPUBCheck(t *testing.T) {
	jar := os.Getenv("EPUBCHECK_JAR")
	if jar == "" {
		t.Skip("EPUBCHECK_JAR not set")
	}
	data, err := epub.New().Render(context.Background(), book())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("java", "-jar", jar, path).CombinedOutput()
	if err != nil {
		t.Fatalf("epubcheck failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func bookWithFigures(t *testing.T) publishing.Ebook {
	t.Helper()
	var jb, pb bytes.Buffer
	_ = jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil)
	_ = png.Encode(&pb, image.NewRGBA(image.Rect(0, 0, 300, 300)))
	b := book()
	b.Figures = []reading.Figure{
		{Ordinal: 0, AfterParagraph: -1, InText: true, Lead: true, Caption: "冒頭", Alt: "会見の様子", MediaType: "image/jpeg", Data: jb.Bytes()},
		{Ordinal: 1, AfterParagraph: 1, InText: true, Caption: "グラフ", MediaType: "image/png", Data: pb.Bytes()},
		{Ordinal: 2, AfterParagraph: 0, InText: false, MediaType: "image/jpeg", Data: jb.Bytes()},
	}
	b.Cover = jb.Bytes()
	return b
}

func TestRenderPlacesFiguresAndDeclaresCover(t *testing.T) {
	out, err := epub.New().Render(context.Background(), bookWithFigures(t))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := unzip(t, out)
	for _, name := range []string{"OEBPS/images/fig-0.jpg", "OEBPS/images/fig-1.png", "OEBPS/images/cover.jpg"} {
		if len(files[name]) == 0 {
			t.Fatalf("missing %s", name)
		}
	}
	if _, ok := files["OEBPS/images/fig-2.jpg"]; ok {
		t.Fatal("a cover-only figure must not be in the book's images")
	}
	art := files["OEBPS/article.xhtml"]
	i0, i1 := strings.Index(art, `src="images/fig-0.jpg"`), strings.Index(art, `src="images/fig-1.png"`)
	p2 := strings.Index(art, book().Article.Paragraphs[1][:9])
	if i0 < 0 || i1 < 0 || i0 >= p2 || p2 >= i1 {
		t.Fatalf("figure order wrong: fig0=%d para2=%d fig1=%d", i0, p2, i1)
	}
	if !strings.Contains(art, `alt="会見の様子"`) || !strings.Contains(art, "<figcaption>") {
		t.Fatal("alt/caption missing")
	}
	opf := files["OEBPS/content.opf"]
	for _, want := range []string{
		`<meta name="cover" content="cover-image"/>`,
		`id="cover-image" href="images/cover.jpg" media-type="image/jpeg" properties="cover-image"`,
		`href="images/fig-1.png" media-type="image/png"`,
	} {
		if !strings.Contains(opf, want) {
			t.Fatalf("content.opf missing %s", want)
		}
	}
	if strings.Contains(opf[strings.Index(opf, "<spine"):], "cover") {
		t.Fatal("the cover must not be in the spine (Kindle would show it twice)")
	}
}

func TestRenderWithFiguresIsDeterministic(t *testing.T) {
	a, _ := epub.New().Render(context.Background(), bookWithFigures(t))
	b, _ := epub.New().Render(context.Background(), bookWithFigures(t))
	if !bytes.Equal(a, b) {
		t.Fatal("not deterministic")
	}
	// make epubcheck-sample renders the book to a file for the validator.
	if p := os.Getenv("EPUB_SAMPLE_OUT"); p != "" {
		if err := os.WriteFile(p, a, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
