// Package feedback is the application-layer feedback pipeline: it
// authorizes a review request against the caller's session and
// document, asks the Teacher agent for corrections, persists the
// result, and records the learning events the rest of the system (and
// Task 14's statistics) reacts to.
package feedback

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/diff"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// contextWindow is how many runes of surrounding document text are
// included on each side of the selection when asking the Teacher agent
// to review it.
const contextWindow = 200

// ErrInvalidSelection is returned when Request's Start/End don't
// satisfy 0 <= Start <= End <= len(document runes).
var ErrInvalidSelection = errors.New("feedback: invalid selection range")

// ErrInvalidStatus is returned when SetCorrectionStatus is asked to set
// anything other than "accepted" or "rejected".
var ErrInvalidStatus = errors.New(`feedback: status must be "accepted" or "rejected"`)

type Service struct {
	sessions storage.SessionRepository
	docs     storage.DocumentRepository
	repo     storage.FeedbackRepository
	grammar  storage.GrammarRepository
	teacher  *teacher.Agent
	rec      *learning.Recorder
}

func NewService(sessions storage.SessionRepository, docs storage.DocumentRepository, repo storage.FeedbackRepository, grammar storage.GrammarRepository, t *teacher.Agent, rec *learning.Recorder) *Service {
	return &Service{sessions: sessions, docs: docs, repo: repo, grammar: grammar, teacher: t, rec: rec}
}

// Request asks for AI feedback on a slice of a document. Start/End are
// rune offsets into the document; Text is the selection text as the
// client already had it (so the server doesn't have to re-derive it
// from Start/End when the client can supply it directly), used when
// non-empty.
type Request struct {
	Identity   learner.IdentityID
	SessionID  session.ID
	DocumentID writing.DocumentID
	Start, End int
	Text       string
}

// CorrectionView is a correction.Correction with the pipeline's added
// context: its current Status and a rune-level Diff between Original
// and Replacement, ready for a UI to render directly.
type CorrectionView struct {
	correction.Correction
	Status string
	Diff   []diff.Segment
}

// Feedback is the result of a review: the reviewed text, the corrected
// text, a whole-selection diff between them, each individual
// correction (with its own diff), and the AI request that produced it
// (for the observability/ratings pages in later tasks).
type Feedback struct {
	ID          string
	Original    string
	Corrected   string
	Diff        []diff.Segment
	Corrections []CorrectionView
	AIRequestID string
}

// RequestFeedback authorizes req against the caller's session and
// document, asks the Teacher agent to review the selection, persists
// the result, and records feedback.requested followed by one
// correction.presented event per applied correction.
//
// Recorder errors here are treated as hard failures (unlike
// application/writing's autosave, which logs-and-continues): by the
// time RequestFeedback would call rec.Record, the FeedbackRecord and
// its corrections are already durably persisted, but the learner
// model's history is the events, not the row — feedback the learner
// sees with no corresponding event would silently corrupt that
// history. Autosave's log-and-continue is safe because the document
// write is the source of truth and events are a secondary ledger of
// it; feedback has no equivalent "the important part already
// succeeded" argument to fall back on for its own record either, but
// once persisted the record does exist — it's the ledger going out of
// sync with it that's unacceptable, so a Record failure here surfaces
// as an error to the caller rather than being swallowed.
func (s *Service) RequestFeedback(ctx context.Context, req Request) (Feedback, error) {
	sess, err := s.sessions.Get(ctx, req.Identity, req.SessionID)
	if err != nil {
		return Feedback{}, err
	}
	doc, err := s.docs.Get(ctx, req.Identity, req.DocumentID)
	if err != nil {
		return Feedback{}, err
	}

	runes := []rune(doc.Content)
	if req.Start < 0 || req.End < req.Start || req.End > len(runes) {
		return Feedback{}, fmt.Errorf("%w: start=%d end=%d document_runes=%d", ErrInvalidSelection, req.Start, req.End, len(runes))
	}

	start, end := req.Start, req.End
	if start == end {
		// An empty selection (typically: no text highlighted, cursor
		// only) means "review the whole document".
		start, end = 0, len(runes)
	}

	selectionText := req.Text
	if selectionText == "" {
		selectionText = string(runes[start:end])
	}
	contextText := windowContext(runes, start, end, contextWindow)

	// conceptCandidates offers the teacher.feedback.v2 prompt the full
	// catalog of taggable grammar concepts; knownSlugs is the same
	// catalog as a set, used below to decide whether each concept the
	// teacher tags a correction with is resolved (in the catalog) or
	// not (recorded anyway, just unresolved — see
	// storage.FeedbackRepository.InsertCorrectionConcepts).
	concepts, err := s.grammar.ListConcepts(ctx)
	if err != nil {
		return Feedback{}, fmt.Errorf("feedback: list grammar concepts: %w", err)
	}
	conceptCandidates := make([]string, 0, len(concepts))
	knownSlugs := make(map[string]bool, len(concepts))
	for _, c := range concepts {
		conceptCandidates = append(conceptCandidates, fmt.Sprintf("%s — %s", c.Slug, c.Name))
		knownSlugs[c.Slug] = true
	}

	result, resp, err := s.teacher.ReviewWriting(ctx, teacher.ReviewInput{
		Identity:  req.Identity,
		Session:   sess,
		Selection: selectionText,
		Context:   contextText,
		// RecentErrors intentionally left empty in the Phase 1 MVP;
		// Phase 2 fills this from weakness statistics.
		ConceptCandidates: conceptCandidates,
	})
	if err != nil {
		return Feedback{}, err
	}

	feedbackID := uuid.New().String()
	corrRecords := make([]storage.CorrectionRecord, 0, len(result.Corrections))
	for i, c := range result.Corrections {
		corrRecords = append(corrRecords, storage.CorrectionRecord{
			ID:            c.ID,
			FeedbackID:    feedbackID,
			Position:      i,
			Original:      c.Original,
			Replacement:   c.Replacement,
			Type:          string(c.Type),
			Severity:      string(c.Severity),
			ExplanationJA: c.Explanation.JA,
			ExplanationEN: c.Explanation.EN,
			Status:        "presented",
		})
	}

	feedbackRec := storage.FeedbackRecord{
		ID:             feedbackID,
		IdentityID:     req.Identity,
		SessionID:      req.SessionID,
		DocumentID:     req.DocumentID,
		SelectionStart: start,
		SelectionEnd:   end,
		SelectionText:  selectionText,
		CorrectedText:  result.Corrected,
		AIRequestID:    resp.RequestID,
	}
	if err := s.repo.InsertFeedback(ctx, feedbackRec, corrRecords); err != nil {
		return Feedback{}, fmt.Errorf("feedback: persist: %w", err)
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: req.Identity,
		SessionID:  &req.SessionID,
		Type:       event.TypeFeedbackRequested,
		Subject:    string(req.DocumentID),
		Evidence: map[string]any{
			"start":       start,
			"end":         end,
			"corrections": len(result.Corrections),
		},
	}); err != nil {
		return Feedback{}, fmt.Errorf("feedback: record %s: %w", event.TypeFeedbackRequested, err)
	}

	views := make([]CorrectionView, 0, len(result.Corrections))
	for _, c := range result.Corrections {
		if err := s.rec.Record(ctx, event.LearningEvent{
			IdentityID: req.Identity,
			SessionID:  &req.SessionID,
			Type:       event.TypeCorrectionPresented,
			Subject:    c.ID,
			Evidence: map[string]any{
				"type":     string(c.Type),
				"severity": string(c.Severity),
			},
		}); err != nil {
			return Feedback{}, fmt.Errorf("feedback: record %s: %w", event.TypeCorrectionPresented, err)
		}

		if err := s.tagConcepts(ctx, req, c, knownSlugs); err != nil {
			return Feedback{}, err
		}

		views = append(views, CorrectionView{
			Correction: c,
			Status:     "presented",
			Diff:       diff.Runes(c.Original, c.Replacement),
		})
	}

	return Feedback{
		ID:          feedbackID,
		Original:    result.Original,
		Corrected:   result.Corrected,
		Diff:        diff.Runes(result.Original, result.Corrected),
		Corrections: views,
		AIRequestID: resp.RequestID,
	}, nil
}

// tagConcepts persists c's grammar-concept tags (if any) and records
// one grammar.concept.encountered event per RESOLVED slug — a slug the
// teacher tagged that isn't in knownSlugs (a hallucinated or stale
// candidate) is still persisted, with resolved=false, but never gets an
// event: the learner model only reacts to concepts it can actually
// explain via GetConcept, not to whatever string the model happened to
// emit.
func (s *Service) tagConcepts(ctx context.Context, req Request, c correction.Correction, knownSlugs map[string]bool) error {
	if len(c.Concepts) == 0 {
		return nil
	}

	resolved := make(map[string]bool, len(c.Concepts))
	for _, slug := range c.Concepts {
		resolved[slug] = knownSlugs[slug]
	}
	if err := s.repo.InsertCorrectionConcepts(ctx, c.ID, c.Concepts, resolved); err != nil {
		return fmt.Errorf("feedback: persist concepts: %w", err)
	}

	for _, slug := range c.Concepts {
		if !resolved[slug] {
			continue
		}
		if err := s.rec.Record(ctx, event.LearningEvent{
			IdentityID: req.Identity,
			SessionID:  &req.SessionID,
			Type:       event.TypeGrammarConceptEncountered,
			Subject:    slug,
			Evidence: map[string]any{
				"correction_id": c.ID,
				"type":          string(c.Type),
				"severity":      string(c.Severity),
			},
		}); err != nil {
			return fmt.Errorf("feedback: record %s: %w", event.TypeGrammarConceptEncountered, err)
		}
	}
	return nil
}

// SetCorrectionStatus records the learner's accept/reject decision on a
// previously presented correction. repo.UpdateCorrectionStatus is
// identity-scoped (via a join to the owning feedback_requests row), so
// a wrong identity misses with storage.ErrNotFound exactly like the
// other repositories rather than leaking another learner's correction.
func (s *Service) SetCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (CorrectionView, error) {
	if status != "accepted" && status != "rejected" {
		return CorrectionView{}, fmt.Errorf("%w: got %q", ErrInvalidStatus, status)
	}

	rec, err := s.repo.UpdateCorrectionStatus(ctx, identity, correctionID, status)
	if err != nil {
		return CorrectionView{}, err
	}

	evType := event.TypeCorrectionAccepted
	if status == "rejected" {
		evType = event.TypeCorrectionRejected
	}
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		SessionID:  &rec.SessionID,
		Type:       evType,
		Subject:    rec.ID,
		Evidence: map[string]any{
			"type":     rec.Type,
			"severity": rec.Severity,
		},
	}); err != nil {
		return CorrectionView{}, fmt.Errorf("feedback: record %s: %w", evType, err)
	}

	return correctionViewFromRecord(rec), nil
}

func correctionViewFromRecord(rec storage.CorrectionRecord) CorrectionView {
	return CorrectionView{
		Correction: correction.Correction{
			ID:          rec.ID,
			Original:    rec.Original,
			Replacement: rec.Replacement,
			Type:        correction.Type(rec.Type),
			Severity:    correction.Severity(rec.Severity),
			Explanation: correction.Explanation{JA: rec.ExplanationJA, EN: rec.ExplanationEN},
		},
		Status: rec.Status,
		Diff:   diff.Runes(rec.Original, rec.Replacement),
	}
}

// windowContext returns the substring of runes covering [start,end]
// widened by radius on each side, clamped to the document's bounds.
func windowContext(runes []rune, start, end, radius int) string {
	from := start - radius
	if from < 0 {
		from = 0
	}
	to := end + radius
	if to > len(runes) {
		to = len(runes)
	}
	return string(runes[from:to])
}
