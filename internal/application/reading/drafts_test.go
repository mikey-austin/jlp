package reading_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appreading "github.com/mikeyaustin/jlp/internal/application/reading"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	pageOne = "政府が発表した新たな経済対策をめぐり、議論が続いている。\n\n中央銀行は金融引き締めを続けている。"
	pageTwo = "物価高への対応が柱となる見通しだ。"
)

func addPage(t *testing.T, h *harness, url, content, selection string, figs ...reading.FigureDraft) appreading.DraftStatus {
	t.Helper()
	st, err := h.svc.AddDraftPage(context.Background(), me, appreading.DraftPage{
		Meta: reading.DraftMeta{Title: "経済対策", SourceName: "WSJ日本版"}, PageURL: url,
		Content: content, Selection: selection, Figures: figs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDraftCollectsPagesAndSelectionWins(t *testing.T) {
	h := newHarness(t, false)
	addPage(t, h, "https://x/1", pageOne, "")
	st := addPage(t, h, "https://x/2", "ignored 本文です。", "  "+pageTwo+"  ")
	if st.Summary.Pages != 2 || st.Summary.Paragraphs != 3 {
		t.Fatalf("summary = %+v", st.Summary)
	}
	rv, err := h.svc.Draft(context.Background(), me, st.Draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := rv.Blocks[len(rv.Blocks)-1]; last.Text != pageTwo {
		t.Fatalf("selection should win, last block = %q", last.Text)
	}
	again := addPage(t, h, "https://x/2", pageTwo, "")
	if again.Summary != st.Summary {
		t.Fatalf("same page twice: %+v then %+v", st.Summary, again.Summary)
	}
	if got, err := h.svc.ActiveDraft(context.Background(), me); err != nil || got.Draft.ID != st.Draft.ID {
		t.Fatalf("active = %+v %v", got, err)
	}
}

func TestAddDraftPageRefusesEmptyPageAndKeepsTheOld(t *testing.T) {
	h := newHarness(t, false)
	st := addPage(t, h, "https://x/1", pageOne, "")
	_, err := h.svc.AddDraftPage(context.Background(), me, appreading.DraftPage{PageURL: "https://x/1", Content: " \n "})
	if !errors.Is(err, reading.ErrEmptyContent) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := h.svc.ActiveDraft(context.Background(), me); got.Summary != st.Summary {
		t.Fatalf("empty re-add changed the draft: %+v", got.Summary)
	}
}

func TestSendDraftBuildsArticleAndDeletesDraft(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	st := addPage(t, h, "https://x/1", pageOne, "", reading.FigureDraft{
		Data: jpegBytes(t), Lead: true, InText: true, AfterText: "政府が発表した",
	})
	addPage(t, h, "https://x/2", pageTwo, "")
	if st.Summary.Images != 1 {
		t.Fatalf("summary = %+v", st.Summary)
	}
	res, err := h.svc.SendDraft(ctx, me, st.Draft.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"政府が発表した新たな経済対策をめぐり、議論が続いている。", "中央銀行は金融引き締めを続けている。", pageTwo}, "\n\n")
	if got := strings.Join(res.Article.Paragraphs, "\n\n"); got != want {
		t.Fatalf("paragraphs = %q", got)
	}
	figs, err := h.repo.ListFigures(ctx, me, res.Article.ID)
	if err != nil || len(figs) != 1 || figs[0].AfterParagraph != 0 {
		t.Fatalf("figures = %+v %v", figs, err)
	}
	if _, err := h.svc.ActiveDraft(ctx, me); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("draft should be gone, err = %v", err)
	}
}

func TestSendDraftWithEverythingExcludedKeepsDraft(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	st := addPage(t, h, "https://x/1", pageTwo, "")
	rv, _ := h.svc.Draft(ctx, me, st.Draft.ID)
	if _, err := h.svc.SetDraftBlockExcluded(ctx, me, st.Draft.ID, rv.Blocks[0].Seq, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SendDraft(ctx, me, st.Draft.ID, false); !errors.Is(err, reading.ErrEmptyContent) {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.svc.ActiveDraft(ctx, me); err != nil {
		t.Fatalf("draft must be kept: %v", err)
	}
}

func TestSendDraftTooLongKeepsDraft(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	long := strings.Repeat("あ", reading.DefaultMaxArticleRunes/2) + "。\n\n" + strings.Repeat("い", reading.DefaultMaxArticleRunes/2+1) + "。"
	st := addPage(t, h, "https://x/1", long, "")
	rv, err := h.svc.Draft(ctx, me, st.Draft.ID)
	if err != nil || !rv.TooLong || rv.MaxRunes != reading.DefaultMaxArticleRunes {
		t.Fatalf("review = %+v %v", rv.Summary, err)
	}
	if _, err := h.svc.SendDraft(ctx, me, st.Draft.ID, false); !errors.Is(err, reading.ErrArticleTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.svc.ActiveDraft(ctx, me); err != nil {
		t.Fatalf("draft must be kept: %v", err)
	}
}

func TestDiscardAndSweepDrafts(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	st := addPage(t, h, "https://x/1", pageOne, "")
	if err := h.svc.DiscardDraft(ctx, me, st.Draft.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ActiveDraft(ctx, me); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	addPage(t, h, "https://x/1", pageOne, "")
	h.advance(6 * 24 * time.Hour)
	h.svc.SweepDrafts(ctx)
	if _, err := h.svc.ActiveDraft(ctx, me); err != nil {
		t.Fatalf("6-day-old draft swept: %v", err)
	}
	h.advance(2 * 24 * time.Hour)
	h.svc.SweepDrafts(ctx)
	if _, err := h.svc.ActiveDraft(ctx, me); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("8-day-old draft kept: %v", err)
	}
}
