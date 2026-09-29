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
	"log/slog"
	"strings"
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

// ErrCorrectionGated is returned by GenerateFromCorrection when the
// correction is still under Phase 2's socratic active-recall gate (PRD
// §9/§53) — storage.CorrectionRecord.IsGated() true, i.e. it carries a
// hint, is still Status "presented", and hasn't been Revealed. The
// /anki UI only ever offers the 「Ankiカード作成」 button on an accepted
// correction, but nothing at the HTTP layer previously stopped a direct
// POST /corrections/{id}/anki against a still-gated one — that would
// have written Replacement/ExplanationJA/ExplanationEN straight onto the
// generated card's Back, spelling out the answer at /anki, without ever
// flipping Revealed or recording answer.revealed. This is deliberately
// NOT auto-revealed here: silently revealing on the learner's behalf
// would corrupt the same active-recall data in the other direction. The
// caller (the HTTP handler) maps this to 409 Conflict.
var ErrCorrectionGated = errors.New("anki: correction is still gated behind its socratic hint")

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
// refuses with ErrCorrectionGated if rec.IsGated() (see that error's doc
// comment), asks the Anki agent to write one flashcard from it, persists
// the result as a "draft" card, and records anki.card.created.
func (s *Service) GenerateFromCorrection(ctx context.Context, identity learner.IdentityID, correctionID string) (storage.AnkiCard, error) {
	rec, err := s.feedback.GetCorrection(ctx, identity, correctionID)
	if err != nil {
		return storage.AnkiCard{}, err
	}
	if rec.IsGated() {
		return storage.AnkiCard{}, ErrCorrectionGated
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
	// correction.accepted/rejected do — unlike anki.card.exported, which
	// fires from the session-less /anki review queue page and so has no
	// session to scope to.
	//
	// Log-and-continue on a Record failure, not hard-fail: card is
	// already durably persisted above (repo.Insert), and nothing
	// downstream subscribes to anki.card.created (see recordExported's
	// doc comment for the same reasoning) — hard-failing here would
	// return a 500 to the caller for a card that a subsequent /anki page
	// load would show as already created, an inconsistency strictly
	// worse than a missing activity-feed row.
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
		slog.Error("record anki.card.created", "identity", identity, "card", card.ID, "err", err)
	}

	return card, nil
}

// Draft is a card whose text a caller already has — no model call
// needed. SourceType/SourceID identify where it came from, exactly as
// for a correction-sourced card, and are what makes CreateDraft
// idempotent.
type Draft struct {
	SourceType, SourceID string
	Front, Back, Notes   string
}

// CreateDraft queues pre-written cards as "draft" in the review queue,
// for sources that already carry curated card content — a 読解 study
// edition's vocabulary list, where asking the Anki agent to rewrite a
// definition the reading agent just wrote would only add cost and
// drift. Each card still goes through the same draft → approved →
// exported review as a generated one: nothing reaches Anki unreviewed.
//
// Idempotent per (SourceType, SourceID): a draft whose source already
// has a card for this identity, in any status, is skipped — so pressing
// 「Ankiカード作成」 twice, or after rejecting half the cards, never
// resurrects or duplicates them. created is how many were new. One
// anki.card.created event is recorded per new card, session-less (the
// source is not a writing session).
func (s *Service) CreateDraft(ctx context.Context, identity learner.IdentityID, drafts []Draft) (created int, err error) {
	existing, err := s.repo.List(ctx, identity, "")
	if err != nil {
		return 0, fmt.Errorf("anki: list cards: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, c := range existing {
		have[c.SourceType+"\x00"+c.SourceID] = true
	}
	for _, d := range drafts {
		key := d.SourceType + "\x00" + d.SourceID
		if d.SourceType == "" || d.SourceID == "" || d.Front == "" || d.Back == "" || have[key] {
			continue
		}
		have[key] = true
		card := storage.AnkiCard{
			ID:         uuid.New().String(),
			IdentityID: identity,
			SourceType: d.SourceType,
			SourceID:   d.SourceID,
			Front:      d.Front,
			Back:       d.Back,
			Notes:      d.Notes,
			Status:     "draft",
			CreatedAt:  time.Now().UTC(),
		}
		if err := s.repo.Insert(ctx, card); err != nil {
			return created, fmt.Errorf("anki: persist card: %w", err)
		}
		created++
		// Log-and-continue, for GenerateFromCorrection's reason: the card
		// is already durable.
		if err := s.rec.Record(ctx, event.LearningEvent{
			IdentityID: identity,
			Type:       event.TypeAnkiCardCreated,
			Subject:    card.ID,
			Evidence:   map[string]any{"source_type": card.SourceType, "source_id": card.SourceID},
		}); err != nil {
			slog.Error("record anki.card.created", "identity", identity, "card", card.ID, "err", err)
		}
	}
	return created, nil
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
// card, atomically marks exactly those cards "exported" (via
// repo.TakeApprovedForExport — see that method's doc comment for the
// single-transaction/concurrency contract this relies on, per the
// controller's ruling on Phase 3 Task 3's code review), and records one
// anki.card.exported event per card. It returns the TSV bytes plus the
// exported card IDs; a caller with nothing approved gets an empty,
// zero-length TSV back (not an error). Front/Back are sanitized before
// being written — see sanitizeTSVField.
func (s *Service) ExportTSV(ctx context.Context, identity learner.IdentityID) ([]byte, []string, error) {
	cards, err := s.repo.TakeApprovedForExport(ctx, identity, time.Now().UTC())
	if err != nil {
		return nil, nil, fmt.Errorf("anki: take approved cards: %w", err)
	}
	if len(cards) == 0 {
		return []byte{}, nil, nil
	}

	var buf bytes.Buffer
	ids := make([]string, 0, len(cards))
	for _, c := range cards {
		buf.WriteString(sanitizeTSVField(c.Front))
		buf.WriteByte('\t')
		buf.WriteString(sanitizeTSVField(c.Back))
		buf.WriteByte('\n')
		ids = append(ids, c.ID)
	}

	// recordExported logs-and-continues on its own failures (see its doc
	// comment) — by this point the cards are already durably marked
	// exported, so the learner must still get their file.
	s.recordExported(ctx, identity, cards)

	return buf.Bytes(), ids, nil
}

// sanitizeTSVField makes s safe to write as one column of one row of a
// tab-separated file: an embedded tab would otherwise be indistinguishable
// from the column separator (silently shifting every field after it into
// the wrong column for that row), and an embedded newline would end the
// row early, splitting one card into two garbled lines in Anki's importer.
// This is not a hypothetical: the canned anki_card.v1 fixture's own Back
// ("「とても面白かったです」\n\n理由: ...") — and, more importantly, the
// anki.generate.v1 prompt's own instruction to write a 理由 on its own
// line — makes an embedded newline the EXPECTED shape of a real Back, not
// an edge case. \r\n is replaced before the lone \n/\r cases so a Windows-
// style line ending becomes exactly one "<br>", not two. Anki's Basic note
// type renders <br> as a line break in its HTML-capable fields, so this is
// the same visual result the learner would see if newlines survived
// intact — just encoded in a way the TSV format itself can carry safely.
func sanitizeTSVField(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r\n", "<br>")
	s = strings.ReplaceAll(s, "\n", "<br>")
	s = strings.ReplaceAll(s, "\r", "<br>")
	return s
}

// PushToAnkiConnect atomically takes every currently "approved" card
// (repo.TakeApprovedForExport — the same primitive ExportTSV uses,
// marking them "exported" up front, in one transaction) and pushes them
// into a running Anki instance via the wired AnkiConnector (see
// SetConnector), recording anki.card.exported for each once the push
// has actually succeeded. It returns ErrAnkiConnectNotConfigured,
// unchanged, when no connector has been wired — the caller (the /anki
// page's handler) should never reach this in the first place when the
// 「Ankiへ送信」 button is correctly hidden, but the check stays here too
// so the guarantee holds regardless of what the UI does.
//
// Because TakeApprovedForExport marks cards "exported" BEFORE the
// AnkiConnect call (there's no way to keep that atomic AND defer the
// mark until after a real network round trip — see the port's doc
// comment), a failed or partial AddNotes call reverts every taken card
// back to "approved" via repo.UpdateStatus (the same method SetStatus
// uses for an ordinary approve/reject) so a card AnkiConnect never
// actually received is never left silently "exported" — it stays in
// the review queue for the learner to retry, exactly like before this
// method existed in its current (atomic-take) shape. This trades one
// failure mode for a narrower one: if AddNotes fails AND the revert's
// UpdateStatus call ALSO fails (two independent failures), the card is
// stuck "exported" without ever reaching Anki — logged, not silently
// swallowed, but not automatically recovered either. AnkiConnect push
// is dormant by default and unit-tested only in this task (no live Anki
// instance), so this narrow double-failure window is an accepted,
// documented tradeoff rather than something worth more machinery here.
func (s *Service) PushToAnkiConnect(ctx context.Context, identity learner.IdentityID) (int, error) {
	if s.connector == nil {
		return 0, ErrAnkiConnectNotConfigured
	}

	cards, err := s.repo.TakeApprovedForExport(ctx, identity, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("anki: take approved cards: %w", err)
	}
	if len(cards) == 0 {
		return 0, nil
	}

	added, err := s.connector.AddNotes(ctx, cards)
	if err != nil {
		s.revertToApproved(ctx, identity, cards)
		return 0, fmt.Errorf("anki: push to ankiconnect: %w", err)
	}

	// AddNotes reports how many notes Anki actually accepted, but not
	// which ones (AnkiConnect's addNotes response is a parallel array of
	// note IDs / nulls per input — see adapters/ankiconnect's doc
	// comment). Rather than guess which subset succeeded, this method
	// requires EVERY card to have been accepted before treating the
	// batch as exported; a partial success reverts the WHOLE batch back
	// to "approved" (never silently left "exported" for a card Anki
	// didn't actually take) so the learner can retry.
	if added != len(cards) {
		s.revertToApproved(ctx, identity, cards)
		return added, fmt.Errorf("anki: ankiconnect accepted %d of %d cards", added, len(cards))
	}

	// recordExported logs-and-continues on its own failures (see its doc
	// comment) — AnkiConnect has already accepted every card and
	// TakeApprovedForExport already committed the "exported" mark, so
	// the push genuinely succeeded; a recording hiccup must not turn
	// that into a reported failure.
	s.recordExported(ctx, identity, cards)

	return added, nil
}

// revertToApproved undoes a TakeApprovedForExport take for cards whose
// subsequent AnkiConnect push failed or was only partially accepted —
// see PushToAnkiConnect's doc comment for why this, not a DB
// transaction spanning the network call, is how that safety property is
// restored. Failures here are logged, not propagated: the caller
// already has a real error to report (the AddNotes failure or partial
// acceptance), and a revert failure on top of that is the narrow,
// documented double-failure window that same doc comment describes.
func (s *Service) revertToApproved(ctx context.Context, identity learner.IdentityID, cards []storage.AnkiCard) {
	for _, c := range cards {
		if _, err := s.repo.UpdateStatus(ctx, identity, c.ID, "approved"); err != nil {
			slog.Error("revert anki card to approved after failed ankiconnect push", "identity", identity, "card", c.ID, "err", err)
		}
	}
}

// recordExported records one anki.card.exported event per card in
// cards — the shared tail ExportTSV and PushToAnkiConnect both run once
// their respective export has actually happened.
//
// Deliberately log-and-continue, not hard-fail (unlike
// application/feedback.Service's own event-recording, which DOES
// hard-fail — see that package's RequestFeedback doc comment): by the
// time this runs, repo.TakeApprovedForExport has ALREADY durably
// committed the "exported" mark (both callers use it), and
// PushToAnkiConnect additionally only reaches this call once
// AnkiConnect has accepted every card too — the real, valuable
// work is done. Nothing downstream subscribes to anki.card.exported the
// way learnermodel.Updater subscribes to correction.presented/
// grammar.concept.encountered (see cmd/jlp/main.go's bus.Subscribe
// calls), so a missing event here is an activity-feed/audit gap, not a
// correctness gap — the same distinction application/writing.Service.
// Autosave's own doc comment draws for writing.updated. Discarding the
// caller's already-built TSV bytes (ExportTSV) or reporting a
// successful AnkiConnect push as a failure (PushToAnkiConnect) over a
// recording hiccup would be strictly worse than an occasionally-missing
// audit-trail row.
func (s *Service) recordExported(ctx context.Context, identity learner.IdentityID, cards []storage.AnkiCard) {
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
			slog.Error("record anki.card.exported", "identity", identity, "card", c.ID, "err", err)
		}
	}
}
