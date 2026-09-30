package reading

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/reading"
)

// draftTTL is how long an untouched draft lives before the worker's
// sweep removes it: long enough to finish a multi-page read across a
// weekend, short enough that abandoned pages (and their image bytes)
// do not pile up.
const draftTTL = 7 * 24 * time.Hour

// DraftPage is one captured page offered to a draft. Selection, when it
// has any text, is what the learner chose to keep and wins over Content.
type DraftPage struct {
	Meta               reading.DraftMeta
	PageURL            string
	Content, Selection string
	Figures            []reading.FigureDraft
}

// DraftStatus is a draft as the extension sees it after each capture.
// Rejected counts figures the page offered that failed validation.
type DraftStatus struct {
	Draft    reading.DraftInfo
	Summary  reading.DraftSummary
	Rejected int
}

// DraftReview is a draft laid out for the review page. TooLong and
// TooManyImages are what would make a send fail, so the page can say so
// before the learner presses it.
type DraftReview struct {
	Draft                  reading.DraftInfo
	Blocks                 []reading.DraftBlock
	Summary                reading.DraftSummary
	MaxRunes               int
	TooLong, TooManyImages bool
}

// AddDraftPage appends the page to the identity's active draft (or
// replaces it when the same page was captured before).
func (s *Service) AddDraftPage(ctx context.Context, identity learner.IdentityID, p DraftPage) (DraftStatus, error) {
	text := p.Content
	if strings.TrimSpace(p.Selection) != "" {
		text = p.Selection
	}
	blocks, rejected := reading.PageBlocks(p.PageURL, text, p.Figures)
	// A page with nothing in it must not reach the repository: re-adding
	// a known page_url replaces its blocks, so an empty one would wipe it.
	if len(blocks) == 0 {
		return DraftStatus{}, reading.ErrEmptyContent
	}
	meta := p.Meta
	meta.Title = reading.ClipTitle(meta.Title)
	info, err := s.d.Repo.AddDraftPage(ctx, identity, meta, p.PageURL, blocks, s.now())
	if err != nil {
		return DraftStatus{}, err
	}
	all, err := s.d.Repo.ListDraftBlocks(ctx, identity, info.ID, false)
	if err != nil {
		return DraftStatus{}, fmt.Errorf("reading: list draft blocks: %w", err)
	}
	return DraftStatus{Draft: info, Summary: reading.Summarise(all), Rejected: len(rejected)}, nil
}

// ActiveDraft reports the identity's draft; storage.ErrNotFound when none.
func (s *Service) ActiveDraft(ctx context.Context, identity learner.IdentityID) (DraftStatus, error) {
	info, err := s.d.Repo.ActiveDraft(ctx, identity)
	if err != nil {
		return DraftStatus{}, err
	}
	blocks, err := s.d.Repo.ListDraftBlocks(ctx, identity, info.ID, false)
	if err != nil {
		return DraftStatus{}, fmt.Errorf("reading: list draft blocks: %w", err)
	}
	return DraftStatus{Draft: info, Summary: reading.Summarise(blocks)}, nil
}

func (s *Service) maxRunes() int {
	if s.cfg.MaxArticleRunes > 0 {
		return s.cfg.MaxArticleRunes
	}
	return reading.DefaultMaxArticleRunes
}

func (s *Service) Draft(ctx context.Context, identity learner.IdentityID, id string) (DraftReview, error) {
	info, err := s.d.Repo.GetDraft(ctx, identity, id)
	if err != nil {
		return DraftReview{}, err
	}
	blocks, err := s.d.Repo.ListDraftBlocks(ctx, identity, id, false)
	if err != nil {
		return DraftReview{}, fmt.Errorf("reading: list draft blocks: %w", err)
	}
	sum := reading.Summarise(blocks)
	limit := s.maxRunes()
	return DraftReview{
		Draft: info, Blocks: blocks, Summary: sum, MaxRunes: limit,
		TooLong: sum.Chars > limit, TooManyImages: sum.KeptImages > reading.MaxFigures,
	}, nil
}

func (s *Service) DraftImage(ctx context.Context, identity learner.IdentityID, id string, seq int) (reading.Figure, error) {
	return s.d.Repo.DraftImage(ctx, identity, id, seq)
}

func (s *Service) SetDraftBlockExcluded(ctx context.Context, identity learner.IdentityID, id string, seq int, excluded bool) (DraftReview, error) {
	if err := s.d.Repo.SetDraftBlockExcluded(ctx, identity, id, seq, excluded, s.now()); err != nil {
		return DraftReview{}, err
	}
	return s.Draft(ctx, identity, id)
}

func (s *Service) SetDraftTitle(ctx context.Context, identity learner.IdentityID, id, title string) error {
	return s.d.Repo.SetDraftTitle(ctx, identity, id, reading.ClipTitle(title), s.now())
}

// SendDraft turns the kept blocks into one article through Submit, so a
// draft gets the same validation, idempotency and queueing as any other
// article. The draft is kept on every failure, so the learner can fix
// what was wrong and send again.
func (s *Service) SendDraft(ctx context.Context, identity learner.IdentityID, id string, deliver bool) (SubmitResult, error) {
	info, err := s.d.Repo.GetDraft(ctx, identity, id)
	if err != nil {
		return SubmitResult{}, err
	}
	// Figure bytes are needed to store the article's images.
	blocks, err := s.d.Repo.ListDraftBlocks(ctx, identity, id, true)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("reading: list draft blocks: %w", err)
	}
	if reading.Summarise(blocks).Chars > s.maxRunes() {
		return SubmitResult{}, reading.ErrArticleTooLarge
	}
	res, err := s.Submit(ctx, identity, reading.Submission(info.Meta, blocks), SubmitOptions{Deliver: deliver})
	if err != nil {
		return SubmitResult{}, err
	}
	if err := s.d.Repo.DeleteDraft(ctx, identity, id); err != nil {
		// The article exists; a leftover draft is swept after a week.
		slog.Error("reading: delete sent draft", "draft", id, "identity", identity, "err", err)
	}
	return res, nil
}

func (s *Service) DiscardDraft(ctx context.Context, identity learner.IdentityID, id string) error {
	return s.d.Repo.DeleteDraft(ctx, identity, id)
}

// SweepDrafts removes drafts untouched for draftTTL. Failures are logged:
// a missed sweep only delays cleanup to the next tick.
func (s *Service) SweepDrafts(ctx context.Context) {
	n, err := s.d.Repo.SweepDrafts(ctx, s.now().Add(-draftTTL))
	if err != nil {
		slog.Error("reading: sweep drafts", "err", err)
		return
	}
	if n > 0 {
		slog.Info("reading: swept stale drafts", "count", n)
	}
}
