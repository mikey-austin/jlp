package cover

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/math/fixed"

	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

func solid(t *testing.T, c color.Color, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	return img
}

func near(c color.Color, r, g, b uint8) bool {
	cr, cg, cb, _ := c.RGBA()
	d := func(x uint32, y uint8) bool { v := int(x>>8) - int(y); return v > -24 && v < 24 }
	return d(cr, r) && d(cg, g) && d(cb, b)
}

func TestDesignPhotoCover(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Design(context.Background(), publishing.CoverInput{
		Title: "電気料金が「過去最高」の水準へ", Source: "NHKニュース", Date: "2026年9月29日",
		Photo: solid(t, color.RGBA{0, 0, 255, 255}, 800, 300), // wide: must be cropped to fill
	})
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, out)
	if img.Bounds().Dx() != Width || img.Bounds().Dy() != Height {
		t.Fatalf("size = %v", img.Bounds())
	}
	for _, p := range []image.Point{{10, 10}, {Width - 10, 10}, {Width / 2, PhotoHeight - 10}} {
		if !near(img.At(p.X, p.Y), 0, 0, 255) {
			t.Fatalf("photo area at %v = %v, want the photo filling it", p, img.At(p.X, p.Y))
		}
	}
	if !near(img.At(20, Height-20), 0xfa, 0xf7, 0xf2) {
		t.Fatalf("text panel background = %v", img.At(20, Height-20))
	}
}

func TestDesignWithoutPhotoUsesAccentBand(t *testing.T) {
	d, _ := New()
	out, err := d.Design(context.Background(), publishing.CoverInput{Title: "見出し"})
	if err != nil {
		t.Fatal(err)
	}
	if img := decode(t, out); !near(img.At(Width/2, PhotoHeight/2), 0xb7, 0x28, 0x2e) {
		t.Fatalf("band = %v", img.At(Width/2, PhotoHeight/2))
	}
}

func TestDesignRejectsCorruptPhoto(t *testing.T) {
	d, _ := New()
	if _, err := d.Design(context.Background(), publishing.CoverInput{Title: "t", Photo: []byte("nope")}); err == nil {
		t.Fatal("a corrupt photo must be an error so the caller can fall back")
	}
}

func TestDesignIsDeterministic(t *testing.T) {
	d, _ := New()
	in := publishing.CoverInput{Title: "同じ表紙", Source: "S", Date: "D"}
	a, _ := d.Design(context.Background(), in)
	b, _ := d.Design(context.Background(), in)
	if !bytes.Equal(a, b) {
		t.Fatal("same input must give the same bytes (the EPUB is deterministic)")
	}
}

func TestWrapLinesEllipsisAndKinsoku(t *testing.T) {
	d, _ := New()
	face := d.titleFace
	width := fixed.I(600)
	long := strings.Repeat("日本語の見出しがとても長い場合、", 20)
	lines := wrapLines(face, long, width, 4)
	if len(lines) != 4 || !strings.HasSuffix(lines[3], "…") {
		t.Fatalf("lines = %q", lines)
	}
	for _, l := range lines {
		if textWidth(face, l) > width {
			t.Fatalf("line overflows: %q", l)
		}
		if strings.ContainsRune("、。」』）！？ー", []rune(l)[0]) {
			t.Fatalf("line starts with a no-break-before character: %q", l)
		}
	}
	if got := wrapLines(face, "短い", width, 4); len(got) != 1 || got[0] != "短い" {
		t.Fatalf("short = %q", got)
	}
}
