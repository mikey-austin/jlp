//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func draftPage(paras int, withImage bool) []reading.DraftBlock {
	var bs []reading.DraftBlock
	for i := 0; i < paras; i++ {
		bs = append(bs, reading.DraftBlock{Kind: reading.BlockParagraph, Text: fmt.Sprintf("段落%d", i)})
	}
	if withImage {
		bs = append(bs, reading.DraftBlock{Kind: reading.BlockImage, Figure: reading.Figure{
			Caption: "c", Alt: "a", MediaType: "image/png", Width: 2, Height: 3, SHA256: "abc", Data: []byte{1, 2, 3}}})
	}
	return bs
}

func texts(bs []reading.DraftBlock) []string {
	var out []string
	for _, b := range bs {
		if b.Kind == reading.BlockImage {
			out = append(out, "img")
		} else {
			out = append(out, b.Text)
		}
	}
	return out
}

func TestReadingDraftPagesAppendReplaceAndOrder(t *testing.T) {
	r, me, other := readingTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	meta := reading.DraftMeta{Title: "題", SourceName: "S", SourceURL: "https://x/1", Author: "A"}

	d1, err := r.AddDraftPage(ctx, me, meta, "https://x/1", draftPage(2, true), now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Minute)
	d2, err := r.AddDraftPage(ctx, me, reading.DraftMeta{Title: "ignored"}, "https://x/2", draftPage(1, false), later)
	if err != nil {
		t.Fatal(err)
	}
	if d2.ID != d1.ID || d2.Meta.Title != "題" || d2.Meta.Author != "A" {
		t.Errorf("second page: %+v vs %+v", d2, d1)
	}
	active, err := r.ActiveDraft(ctx, me)
	if err != nil || active.ID != d1.ID || !active.UpdatedAt.Equal(later) || !active.CreatedAt.Equal(now) {
		t.Fatalf("active = %+v, %v", active, err)
	}

	bs, err := r.ListDraftBlocks(ctx, me, d1.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(texts(bs)); got != "[段落0 段落1 img 段落0]" {
		t.Errorf("blocks = %s", got)
	}
	if bs[0].Seq != 10000 || bs[2].Seq != 10002 || bs[3].Seq != 20000 || bs[3].Page != 2 || bs[3].PageURL != "https://x/2" {
		t.Errorf("seq/page = %+v", bs)
	}
	if bs[2].Figure.Data != nil || bs[2].Figure.Width != 2 || bs[2].Figure.MediaType != "image/png" || bs[2].Figure.Caption != "c" {
		t.Errorf("listed image = %+v", bs[2].Figure)
	}
	if bs, _ := r.ListDraftBlocks(ctx, me, d1.ID, true); string(bs[2].Figure.Data) != "\x01\x02\x03" {
		t.Errorf("withData image = %+v", bs[2].Figure)
	}

	// Re-adding page 1 replaces it in place; page 2 keeps its number.
	if _, err := r.AddDraftPage(ctx, me, meta, "https://x/1", draftPage(3, false), later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	bs, _ = r.ListDraftBlocks(ctx, me, d1.ID, false)
	if got := fmt.Sprint(texts(bs)); got != "[段落0 段落1 段落2 段落0]" {
		t.Errorf("after replace = %s", got)
	}
	if bs[0].Page != 1 || bs[3].Page != 2 || bs[3].Seq != 20000 {
		t.Errorf("after replace = %+v", bs)
	}
	// An empty page_url is always a new page.
	if _, err := r.AddDraftPage(ctx, me, meta, "", draftPage(1, false), later); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddDraftPage(ctx, me, meta, "", draftPage(1, false), later); err != nil {
		t.Fatal(err)
	}
	if bs, _ = r.ListDraftBlocks(ctx, me, d1.ID, false); len(bs) != 6 || bs[5].Page != 4 {
		t.Errorf("empty-url pages = %+v", bs)
	}

	// Scoping.
	if _, err := r.ActiveDraft(ctx, other); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("other's active = %v", err)
	}
	if _, err := r.GetDraft(ctx, other, d1.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("other's get = %v", err)
	}
	if bs, err := r.ListDraftBlocks(ctx, other, d1.ID, true); err != nil || len(bs) != 0 {
		t.Errorf("other's blocks = %v, %v", bs, err)
	}
	if _, err := r.GetDraft(ctx, me, "not-a-uuid"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("bad id = %v", err)
	}
}

func TestReadingDraftFullAtTenPages(t *testing.T) {
	r, me, _ := readingTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var d reading.DraftInfo
	for i := 0; i < reading.MaxDraftPages; i++ {
		var err error
		if d, err = r.AddDraftPage(ctx, me, reading.DraftMeta{}, fmt.Sprintf("https://x/%d", i), draftPage(1, false), now); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(time.Hour)
	if _, err := r.AddDraftPage(ctx, me, reading.DraftMeta{}, "https://x/new", draftPage(1, false), later); !errors.Is(err, reading.ErrDraftFull) {
		t.Fatalf("11th page: %v", err)
	}
	if _, err := r.AddDraftPage(ctx, me, reading.DraftMeta{}, "", draftPage(1, false), later); !errors.Is(err, reading.ErrDraftFull) {
		t.Fatalf("11th empty-url page: %v", err)
	}
	bs, _ := r.ListDraftBlocks(ctx, me, d.ID, false)
	if a, _ := r.ActiveDraft(ctx, me); len(bs) != 10 || !a.UpdatedAt.Equal(now) {
		t.Errorf("a refused page changed the draft: %d blocks, %+v", len(bs), a)
	}
	// A replace is not a new page.
	if _, err := r.AddDraftPage(ctx, me, reading.DraftMeta{}, "https://x/3", draftPage(2, false), later); err != nil {
		t.Errorf("replace at full: %v", err)
	}
}

func TestReadingDraftEditsImagesAndSweep(t *testing.T) {
	r, me, other := readingTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	d, err := r.AddDraftPage(ctx, me, reading.DraftMeta{Title: "旧"}, "https://x/1", draftPage(1, true), now)
	if err != nil {
		t.Fatal(err)
	}
	t1 := now.Add(time.Minute)
	if err := r.SetDraftBlockExcluded(ctx, me, d.ID, 10000, true, t1); err != nil {
		t.Fatal(err)
	}
	bs, _ := r.ListDraftBlocks(ctx, me, d.ID, false)
	if !bs[0].Excluded || bs[1].Excluded {
		t.Errorf("excluded = %+v", bs)
	}
	if got, _ := r.GetDraft(ctx, me, d.ID); !got.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at = %v", got.UpdatedAt)
	}
	if err := r.SetDraftBlockExcluded(ctx, me, d.ID, 10000, false, t1); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"other identity": r.SetDraftBlockExcluded(ctx, other, d.ID, 10000, true, t1),
		"no such seq":    r.SetDraftBlockExcluded(ctx, me, d.ID, 5, true, t1),
		"other title":    r.SetDraftTitle(ctx, other, d.ID, "x", t1),
		"other delete":   r.DeleteDraft(ctx, other, d.ID),
	} {
		if !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := r.SetDraftTitle(ctx, me, d.ID, "新", t1); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetDraft(ctx, me, d.ID); got.Meta.Title != "新" {
		t.Errorf("title = %q", got.Meta.Title)
	}

	img, err := r.DraftImage(ctx, me, d.ID, 10001)
	if err != nil || string(img.Data) != "\x01\x02\x03" || img.MediaType != "image/png" || img.Width != 2 || img.Height != 3 {
		t.Fatalf("image = %+v, %v", img, err)
	}
	if _, err := r.DraftImage(ctx, me, d.ID, 10000); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("paragraph as image: %v", err)
	}
	if _, err := r.DraftImage(ctx, other, d.ID, 10001); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("other's image: %v", err)
	}

	// Sweep: a draft untouched before the cutoff goes, blocks and all.
	// The dev database is shared, so age this draft into 2000 and sweep
	// only that far back rather than sweeping anyone's live draft.
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := r.SetDraftTitle(ctx, me, d.ID, "新", old); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SweepDrafts(ctx, old); err != nil { // strictly before: the boundary stays
		t.Fatal(err)
	}
	if _, err := r.GetDraft(ctx, me, d.ID); err != nil {
		t.Fatalf("draft at the cutoff was swept: %v", err)
	}
	if n, err := r.SweepDrafts(ctx, old.Add(time.Hour)); err != nil || n < 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if _, err := r.GetDraft(ctx, me, d.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("swept draft: %v", err)
	}
	var blocks int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reading_draft_blocks WHERE draft_id = $1`, d.ID).Scan(&blocks); err != nil || blocks != 0 {
		t.Errorf("blocks after sweep = %d, %v", blocks, err)
	}

	d, _ = r.AddDraftPage(ctx, me, reading.DraftMeta{}, "", draftPage(1, false), now)
	if err := r.DeleteDraft(ctx, me, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ActiveDraft(ctx, me); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}
