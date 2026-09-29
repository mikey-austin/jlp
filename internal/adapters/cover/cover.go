// Package cover draws a 読解 edition's cover: the article's lead photo
// over a typeset title, as one JPEG, so the title shows even in a
// Kindle library thumbnail. Fonts are embedded (M PLUS 1p, OFL — see
// fonts/OFL.txt); nothing is fetched.
package cover

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/mikeyaustin/jlp/internal/ports/publishing"
)

// The licence travels inside the binary with the fonts it covers.
//
//go:embed fonts/MPLUS1p-Bold.ttf fonts/MPLUS1p-Regular.ttf fonts/OFL.txt
var fonts embed.FS

// Geometry: Amazon's recommended 1:1.6, photo on the top 60%.
const (
	Width       = 1600
	Height      = 2560
	PhotoHeight = 1536
	margin      = 120
	titleSize   = 108
	minCrop     = 8 // smaller than this is not a picture, whatever its bytes say
)

var (
	accent = color.RGBA{0xb7, 0x28, 0x2e, 0xff} // the app's accent
	paper  = color.RGBA{0xfa, 0xf7, 0xf2, 0xff}
	ink    = color.RGBA{0x1f, 0x1b, 0x16, 0xff}
	muted  = color.RGBA{0x6b, 0x63, 0x5a, 0xff}
)

// Designer implements publishing.CoverDesigner.
type Designer struct {
	// mu serializes Design: an opentype face caches glyphs and is not safe
	// for concurrent use, and covers are rare and quick.
	mu                              sync.Mutex
	titleFace, kickerFace, metaFace font.Face
}

var _ publishing.CoverDesigner = (*Designer)(nil)

// New parses the embedded fonts once.
func New() (*Designer, error) {
	load := func(name string, size float64) (font.Face, error) {
		b, err := fonts.ReadFile("fonts/" + name)
		if err != nil {
			return nil, err
		}
		f, err := opentype.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("cover: parse %s: %w", name, err)
		}
		return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	var d Designer
	var err error
	if d.titleFace, err = load("MPLUS1p-Bold.ttf", titleSize); err != nil {
		return nil, err
	}
	if d.kickerFace, err = load("MPLUS1p-Bold.ttf", 52); err != nil {
		return nil, err
	}
	if d.metaFace, err = load("MPLUS1p-Regular.ttf", 50); err != nil {
		return nil, err
	}
	return &d, nil
}

// Design draws the cover. A photo that cannot be decoded is an error, so
// the caller can retry without it.
func (d *Designer) Design(_ context.Context, in publishing.CoverInput) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	xdraw.Draw(img, img.Bounds(), image.NewUniform(paper), image.Point{}, xdraw.Src)
	top := image.Rect(0, 0, Width, PhotoHeight)
	if in.Photo != nil {
		photo, _, err := image.Decode(bytes.NewReader(in.Photo))
		if err != nil {
			return nil, fmt.Errorf("cover: decode photo: %w", err)
		}
		crop := cropToFill(photo.Bounds(), top.Dx(), top.Dy())
		if crop.Dx() < minCrop || crop.Dy() < minCrop {
			// An extreme aspect ratio crops to nothing (or a pixel); treat it like an
			// undecodable photo so the caller falls back.
			return nil, fmt.Errorf("cover: photo %v cannot fill the cover", photo.Bounds())
		}
		// Over, on the paper already drawn, so transparency shows paper
		// rather than the black JPEG makes of alpha 0.
		xdraw.CatmullRom.Scale(img, top, photo, crop, xdraw.Over, nil)
	} else {
		xdraw.Draw(img, top, image.NewUniform(accent), image.Point{}, xdraw.Src)
	}

	text := func(face font.Face, c color.Color, s string, y int) {
		(&font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face, Dot: fixed.P(margin, y)}).DrawString(s)
	}
	y := PhotoHeight + 130
	text(d.kickerFace, accent, "日本語読解", y)
	y += 40
	// The font's own line height is far looser than a headline wants (and
	// four such lines would run into the meta line), so lead from the size.
	lh := titleSize * 6 / 5
	for _, line := range wrapLines(d.titleFace, in.Title, fixed.I(Width-2*margin), 4) {
		y += lh
		text(d.titleFace, ink, line, y)
	}
	meta := in.Source
	if in.Date != "" {
		if meta != "" {
			meta += "・"
		}
		meta += in.Date
	}
	if meta != "" {
		text(d.metaFace, muted, meta, Height-150)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// cropToFill returns the centred part of src with the target's aspect.
func cropToFill(src image.Rectangle, w, h int) image.Rectangle {
	sw, sh := src.Dx(), src.Dy()
	if sw*h > sh*w { // too wide
		cw := sh * w / h
		x := src.Min.X + (sw-cw)/2
		return image.Rect(x, src.Min.Y, x+cw, src.Max.Y)
	}
	ch := sw * h / w
	y := src.Min.Y + (sh-ch)/2
	return image.Rect(src.Min.X, y, src.Max.X, y+ch)
}
