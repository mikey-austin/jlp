// Package anki is the application-layer Anki review queue pipeline
// (PRD §19): it turns an accepted correction into a draft flashcard via
// the Anki agent, lets a learner approve/reject drafts, and exports the
// approved set — as a downloadable TSV file, or (when configured) by
// pushing directly to a running Anki instance over AnkiConnect.
package anki

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/agent/anki"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ErrInvalidStatus is returned when SetStatus is asked to set anything
// other than "approved" or "rejected" — mirrors
// application/feedback.Service's ErrInvalidStatus for the analogous
// correction accept/reject validation.
var ErrInvalidStatus = errors.New(`anki: status must be "approved" or "rejected"`)

// ErrAnkiConnectNotConfigured is returned by PushToAnkiConnect when no
// AnkiConnector has been wired in (see SetConnector's doc comment): the
// operator hasn't set APP_ANKI_CONNECT_URL, so this feature stays
// dormant — the caller (the /anki page's handler) uses this to decide
// whether to even show the 「Ankiへ送信」 button, and this error is the
// same signal if the button is somehow reached anyway (e.g. a stale
// page).
var ErrAnkiConnectNotConfigured = errors.New("anki: AnkiConnect is not configured")

// sourceTypeCorrection is the only AnkiCard.SourceType
// GenerateFromCorrection ever produces — PRD §19 lists other future
// sources (recurring grammar problems, vocabulary, tutor lessons,
// successful examples), but Phase 3 Task 3 only wires up the
// correction path.
const sourceTypeCorrection = "correction"

// AnkiConnector is the minimal capability PushToAnkiConnect needs from
// an AnkiConnect client. It's declared here, in the CONSUMING package,
// rather than imported from adapters/ankiconnect: application code may
// depend on ports, never on adapters (PRD §75 Rule 3) — a Go interface
// declared where it's used, satisfied structurally by
// *ankiconnect.Client without either package importing the other, is
// the standard way to keep that direction intact while still letting
// main.go wire the concrete adapter in (see SetConnector).
type AnkiConnector interface {
	AddNotes(ctx context.Context, cards []storage.AnkiCard) (int, error)
}

// Service wires the Anki review queue pipeline.
type Service struct {
	repo      storage.AnkiCardRepository
	feedback  storage.FeedbackRepository
	agent     *anki.Agent
	rec       *learning.Recorder
	connector AnkiConnector
}

// NewService wires the Anki pipeline. connector starts nil (see
// SetConnector) — PushToAnkiConnect is dormant until main.go calls
// SetConnector, exactly when APP_ANKI_CONNECT_URL is configured.
func NewService(repo storage.AnkiCardRepository, feedback storage.FeedbackRepository, agent *anki.Agent, rec *learning.Recorder) *Service {
	return &Service{repo: repo, feedback: feedback, agent: agent, rec: rec}
}

// SetConnector wires an AnkiConnector into s: from this call on,
// PushToAnkiConnect actually pushes. Mirrors
// application/learnermodel.Updater.SetPlanner's optional, late-bound
// wiring shape — main.go only calls this when cfg.Anki.ConnectURL is
// non-empty, so an unconfigured deployment's Service simply never gets
// one, and PushToAnkiConnect fails fast with
// ErrAnkiConnectNotConfigured instead of attempting a request to a
// nonexistent local Anki instance.
func (s *Service) SetConnector(c AnkiConnector) {
	s.connector = c
}

// GenerateFromCorrection reads correctionID's stored context (identity-
// scoped via storage.FeedbackRepository.GetCorrection — a wrong
// identity or unknown correction both miss with storage.ErrNotFound),
// asks the Anki agent to write one flashcard from it, persists the
// result as a "draft" card, and records anki.card.created.
func (s *Service) GenerateFromCorrection(ctx context.Context, identity learner.IdentityID, correctionID string) (storage.AnkiCard, error) {
	rec, err := s.feedback.GetCorrection(ctx, identity, correctionID)
	if err != nil {
		return storage.AnkiCard{}, err
	}

	front, back, notes, _, err := s.agent.Generate(ctx, anki.GenerateInput{
		Identity:      identity,
		SourceType:    sourceTypeCorrection,
		SourceText:    rec.Original,
		CorrectedText: rec.Replacement,
		ExplanationJA: rec.ExplanationJA,
		ExplanationEN: rec.ExplanationEN,
	})
	if err != nil {
		return storage.AnkiCard{}, fmt.Errorf("anki: generate card: %w", err)
	}

	card := storage.AnkiCard{
		ID:         uuid.New().String(),
		IdentityID: identity,
		SourceType: sourceTypeCorrection,
		SourceID:   correctionID,
		Front:      front,
		Back:       back,
		Notes:      notes,
		Status:     "draft",
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.repo.Insert(ctx, card); err != nil {
		return storage.AnkiCard{}, fmt.Errorf("anki: persist card: %w", err)
	}

	// SessionID comes from rec (the correction's owning session, via
	// GetCorrection's join) rather than left nil: anki.card.created is
	// triggered from within that session's workspace (the
	// 「Ankiカード作成」 button lives on a correction card there), so it
	// belongs in that session's activity feed exactly like
	// correction.accepted/rejected do — unlike anki.card.exported below,
	// which fires from the session-less /anki review queue page and so
	// has no session to scope to.
	sessionID := rec.SessionID
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		SessionID:  &sessionID,
		Type:       event.TypeAnkiCardCreated,
		Subject:    card.ID,
		Evidence: map[string]any{
			"source_type": card.SourceType,
			"source_id":   card.SourceID,
		},
	}); err != nil {
		return storage.AnkiCard{}, fmt.Errorf("anki: record %s: %w", event.TypeAnkiCardCreated, err)
	}

	return card, nil
}

// SetStatus records the learner's approve/reject decision on a
// previously generated draft card. repo.UpdateStatus is identity-
// scoped, so a wrong identity misses with storage.ErrNotFound exactly
// like application/feedback.Service.SetCorrectionStatus.
func (s *Service) SetStatus(ctx context.Context, identity learner.IdentityID, cardID, status string) (storage.AnkiCard, error) {
	if status != "approved" && status != "rejected" {
		return storage.AnkiCard{}, fmt.Errorf("%w: got %q", ErrInvalidStatus, status)
	}
	return s.repo.UpdateStatus(ctx, identity, cardID, status)
}

// ExportTSV builds a `front\tback\n`-per-row, no-header TSV (Anki's
// default plain-text import format) from every currently "approved"
// card, marks exactly those cards "exported", and records one
// anki.card.exported event per card. It returns the TSV bytes plus the
// exported card IDs; a caller with nothing approved gets an empty,
// zero-length TSV back (not an error).
//
// Atomicity note: repo.ApprovedForExport and repo.MarkExported are two
// separate calls (see storage.AnkiCardRepository — MarkExported also
// backs PushToAnkiConnect's read-then-push-then-mark flow, where a real
// network call to AnkiConnect necessarily sits BETWEEN the read and the
// mark, so the two could never share one SQL transaction anyway). For
// THIS caller specifically there's no such gap: MarkExported is called
// immediately afterward with exactly the ids ApprovedForExport just
// returned — never a broader "mark everything approved" — and
// repo.MarkExported's own WHERE clause is restricted to still-
// "approved" rows (idempotent). That closes the practical risk this
// method's "no double export on an immediate second call" contract
// cares about: by the time a second, later ExportTSV call's own read
// runs, the first call's mark has already committed. A genuinely
// concurrent pair of calls from the same identity remains a narrow,
// accepted race in this single-operator app — true row-level locking
// across both statements would need a repository method this port
// doesn't expose.
func (s *Service) ExportTSV(ctx context.Context, identity learner.IdentityID) ([]byte, []string, error) {
	cards, err := s.repo.ApprovedForExport(ctx, identity)
	if err != nil {
		return nil, nil, fmt.Errorf("anki: list approved cards: %w", err)
	}
	if len(cards) == 0 {
		return []byte{}, nil, nil
	}

	var buf bytes.Buffer
	ids := make([]string, 0, len(cards))
	for _, c := range cards {
		buf.WriteString(c.Front)
		buf.WriteByte('\t')
		buf.WriteString(c.Back)
		buf.WriteByte('\n')
		ids = append(ids, c.ID)
	}

	if err := s.repo.MarkExported(ctx, identity, ids); err != nil {
		return nil, nil, fmt.Errorf("anki: mark exported: %w", err)
	}

	if err := s.recordExported(ctx, identity, cards); err != nil {
		return nil, nil, err
	}

	return buf.Bytes(), ids, nil
}

// PushToAnkiConnect pushes every currently "approved" card straight
// into a running Anki instance via the wired AnkiConnector (see
// SetConnector), then marks the ones AddNotes actually reports having
// added as "exported" and records anki.card.exported for each. It
// returns ErrAnkiConnectNotConfigured, unchanged, when no connector has
// been wired — the caller (the /anki page's handler) should never reach
// this in the first place when the 「Ankiへ送信」 button is correctly
// hidden, but the check stays here too so the guarantee holds
// regardless of what the UI does.
func (s *Service) PushToAnkiConnect(ctx context.Context, identity learner.IdentityID) (int, error) {
	if s.connector == nil {
		return 0, ErrAnkiConnectNotConfigured
	}

	cards, err := s.repo.ApprovedForExport(ctx, identity)
	if err != nil {
		return 0, fmt.Errorf("anki: list approved cards: %w", err)
	}
	if len(cards) == 0 {
		return 0, nil
	}

	added, err := s.connector.AddNotes(ctx, cards)
	if err != nil {
		return 0, fmt.Errorf("anki: push to ankiconnect: %w", err)
	}

	// AddNotes reports how many notes Anki actually accepted, but not
	// which ones (AnkiConnect's addNotes response is a parallel array of
	// note IDs / nulls per input — see adapters/ankiconnect's doc
	// comment). Rather than guess which subset succeeded, this method
	// only marks the whole batch exported when EVERY card was accepted;
	// a partial success is surfaced as an error so the learner can retry
	// (approved cards stay approved, never silently lost) instead of
	// mismarking cards Anki never actually received.
	if added != len(cards) {
		return added, fmt.Errorf("anki: ankiconnect accepted %d of %d cards", added, len(cards))
	}

	ids := make([]string, 0, len(cards))
	for _, c := range cards {
		ids = append(ids, c.ID)
	}
	if err := s.repo.MarkExported(ctx, identity, ids); err != nil {
		return added, fmt.Errorf("anki: mark exported: %w", err)
	}

	if err := s.recordExported(ctx, identity, cards); err != nil {
		return added, err
	}

	return added, nil
}

// recordExported records one anki.card.exported event per card in
// cards — the shared tail ExportTSV and PushToAnkiConnect both run once
// their respective export has actually happened.
func (s *Service) recordExported(ctx context.Context, identity learner.IdentityID, cards []storage.AnkiCard) error {
	for _, c := range cards {
		if err := s.rec.Record(ctx, event.LearningEvent{
			IdentityID: identity,
			Type:       event.TypeAnkiCardExported,
			Subject:    c.ID,
			Evidence: map[string]any{
				"source_type": c.SourceType,
				"source_id":   c.SourceID,
			},
		}); err != nil {
			return fmt.Errorf("anki: record %s: %w", event.TypeAnkiCardExported, err)
		}
	}
	return nil
}
