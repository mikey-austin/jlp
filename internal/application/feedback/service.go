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
	"log/slog"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
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

// recentErrorsLimit caps how many of the learner's top priorities feed
// the Teacher prompt's RecentErrors — PRD §16's adapt loop: enough to
// steer severity/emphasis without crowding out the rest of the prompt.
const recentErrorsLimit = 5

// activationCandidatesLimit caps how many activation candidates feed
// the Teacher prompt's ExpressionsToEncourage — PRD §55/§17.5's
// vocabulary activator, the brief's "ActivationCandidates(5)".
const activationCandidatesLimit = 5

// ErrInvalidSelection is returned when Request's Start/End don't
// satisfy 0 <= Start <= End <= len(document runes).
var ErrInvalidSelection = errors.New("feedback: invalid selection range")

// ErrInvalidStatus is returned when SetCorrectionStatus is asked to set
// anything other than "accepted" or "rejected".
var ErrInvalidStatus = errors.New(`feedback: status must be "accepted" or "rejected"`)

type Service struct {
	sessions   storage.SessionRepository
	docs       storage.DocumentRepository
	repo       storage.FeedbackRepository
	grammar    storage.GrammarRepository
	priorities storage.PriorityRepository
	planner    *planner.Planner
	vocab      *appvocabulary.Service
	teacher    *teacher.Agent
	rec        *learning.Recorder
}

// NewService wires the feedback pipeline. priorities feeds the Teacher
// prompt's RecentErrors (see RequestFeedback below) — the service reads
// storage.PriorityRepository.Top directly rather than going through
// planner, deliberately keeping the planner's own Recompute off the
// request path: priorities are kept fresh by the learnermodel Updater
// (see that package's SetPlanner) reacting to the PRECEDING request's
// events, not recomputed synchronously on every review.
//
// planner IS used directly, though, for its ActivationCandidates method
// (PRD §55/§17.5's vocabulary activator — see expressionsToEncourage
// below): unlike Recompute, ActivationCandidates is a thin,
// stateless passthrough over storage.VocabularyRepository.List — one
// indexed SELECT plus an in-memory sort, no derived state to keep
// fresh — so calling it live on every request costs nothing Recompute's
// off-request-path treatment was guarding against.
//
// vocab.DetectProduction runs at the very end of every RequestFeedback
// call — see that method's closing comment for why it's non-fatal.
func NewService(sessions storage.SessionRepository, docs storage.DocumentRepository, repo storage.FeedbackRepository, grammar storage.GrammarRepository, priorities storage.PriorityRepository, plnr *planner.Planner, vocab *appvocabulary.Service, t *teacher.Agent, rec *learning.Recorder) *Service {
	return &Service{sessions: sessions, docs: docs, repo: repo, grammar: grammar, priorities: priorities, planner: plnr, vocab: vocab, teacher: t, rec: rec}
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

	recentErrors, err := s.recentErrors(ctx, req.Identity)
	if err != nil {
		return Feedback{}, fmt.Errorf("feedback: list priorities: %w", err)
	}

	expressionsToEncourage, err := s.expressionsToEncourage(ctx, req.Identity)
	if err != nil {
		// Phrased distinctly from planner.ActivationCandidates' own
		// "planner: list activation candidates: %w" wrap, so the two
		// don't stutter into "feedback: list activation candidates:
		// planner: list activation candidates: ...".
		return Feedback{}, fmt.Errorf("feedback: expressions to encourage: %w", err)
	}

	result, resp, err := s.teacher.ReviewWriting(ctx, teacher.ReviewInput{
		Identity:               req.Identity,
		Session:                sess,
		Selection:              selectionText,
		Context:                contextText,
		RecentErrors:           recentErrors,
		ConceptCandidates:      conceptCandidates,
		ExpressionsToEncourage: expressionsToEncourage,
	})
	if err != nil {
		return Feedback{}, err
	}

	feedbackID := uuid.New().String()
	corrRecords := make([]storage.CorrectionRecord, 0, len(result.Corrections))
	// conceptTags (keyed by correction ID) is what InsertFeedback
	// persists to correction_concepts, in the SAME transaction as
	// feedback_requests/corrections; resolvedConcepts (also keyed by
	// correction ID) is the deduped, RESOLVED-only subset — the single
	// source of truth this function uses for BOTH the initial
	// CorrectionView.Concepts the caller sees and which slugs get a
	// grammar.concept.encountered event below. Computing both here,
	// before InsertFeedback, means the view can never show a concept
	// SetCorrectionStatus's later GetCorrectionConcepts-backed view
	// wouldn't also show (an unresolved or duplicate slug), and vice
	// versa — one codepath decides "what counts as this correction's
	// concepts," not two.
	conceptTags := make(map[string][]storage.ConceptTag, len(result.Corrections))
	resolvedConcepts := make(map[string][]string, len(result.Corrections))
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
		if len(c.Concepts) > 0 {
			tags, resolved := resolveConceptTags(c.Concepts, knownSlugs)
			conceptTags[c.ID] = tags
			resolvedConcepts[c.ID] = resolved
		}
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
	if err := s.repo.InsertFeedback(ctx, feedbackRec, corrRecords, conceptTags); err != nil {
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

		// One grammar.concept.encountered event per RESOLVED slug — a
		// slug the teacher tagged that isn't in knownSlugs (a
		// hallucinated or stale candidate) was still persisted above
		// (resolved=false), but never gets an event: the learner model
		// only reacts to concepts it can actually explain via
		// GetConcept, not to whatever string the model happened to
		// emit. Recorder errors here stay hard-fail, same as every
		// other event in this method.
		for _, slug := range resolvedConcepts[c.ID] {
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
				return Feedback{}, fmt.Errorf("feedback: record %s: %w", event.TypeGrammarConceptEncountered, err)
			}
		}

		// cv is a copy of c with Concepts narrowed to resolvedConcepts:
		// the raw c.Concepts from the AI result can contain unresolved
		// slugs (which 404 at /grammar/{slug}) and pre-dedup duplicates —
		// exactly the mismatch SetCorrectionStatus's
		// GetCorrectionConcepts-backed view would NOT have shown. Using
		// resolvedConcepts here instead of c.Concepts keeps the initial
		// view and the post-accept/reject view in agreement about what
		// a correction's concepts are.
		cv := c
		cv.Concepts = resolvedConcepts[c.ID]
		views = append(views, CorrectionView{
			Correction: cv,
			Status:     "presented",
			Diff:       diff.Runes(c.Original, c.Replacement),
		})
	}

	// Production detection runs LAST, after every hard-fail event above
	// has already committed, and its error is deliberately swallowed
	// (logged, not returned): unlike feedback.requested/
	// correction.presented/grammar.concept.encountered — each of which
	// IS this round's core review history, so a Recorder failure for
	// any of them must surface as an error rather than silently drift
	// the event log out of sync with the already-persisted
	// FeedbackRecord (see this method's opening doc comment) — a
	// vocabulary production is enrichment layered on TOP of that
	// history, not part of it: whether or not 取り組む got noticed as
	// "produced" this round has no bearing on whether the review
	// itself succeeded, and failing (or partially failing) the whole
	// request over it would make an unrelated subsystem's hiccup look
	// like the Teacher agent itself failed. correctedSpans is every
	// presented correction's Original — see
	// appvocabulary.Service.DetectProduction's doc comment for exactly
	// how those decide successful vs. not — and text is selectionText,
	// what the learner actually wrote, not the corrected result.
	if s.vocab != nil {
		correctedSpans := make([]string, 0, len(result.Corrections))
		for _, c := range result.Corrections {
			correctedSpans = append(correctedSpans, c.Original)
		}
		if err := s.vocab.DetectProduction(ctx, req.Identity, req.SessionID, selectionText, correctedSpans); err != nil {
			slog.Error("vocabulary detect production", "err", err)
		}
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

// resolveConceptTags dedupes slugs (first-occurrence order) and
// classifies each against knownSlugs, returning both the full set of
// storage.ConceptTag rows to persist (resolved and unresolved alike —
// see storage.FeedbackRepository's doc comment on why an unknown slug
// is still recorded) and the subset that resolved, in the same order.
// Callers must treat the second return value as the single source of
// truth for "this correction's concepts" — never use the raw, un-deduped,
// unfiltered slug list a caller happened to be given.
func resolveConceptTags(slugs []string, knownSlugs map[string]bool) (tags []storage.ConceptTag, resolved []string) {
	deduped := dedupeSlugs(slugs)
	tags = make([]storage.ConceptTag, 0, len(deduped))
	resolved = make([]string, 0, len(deduped))
	for _, slug := range deduped {
		ok := knownSlugs[slug]
		tags = append(tags, storage.ConceptTag{Slug: slug, Resolved: ok})
		if ok {
			resolved = append(resolved, slug)
		}
	}
	return tags, resolved
}

// dedupeSlugs returns slugs with duplicates removed, preserving
// first-occurrence order.
func dedupeSlugs(slugs []string) []string {
	seen := make(map[string]bool, len(slugs))
	out := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
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

	// rec (from storage.CorrectionRecord) carries no Concepts field —
	// tags live in correction_concepts, not corrections — so the
	// re-rendered card after an accept/reject would otherwise silently
	// drop its concept chip(s). Fetch them back explicitly.
	concepts, err := s.repo.GetCorrectionConcepts(ctx, rec.ID)
	if err != nil {
		return CorrectionView{}, fmt.Errorf("feedback: get correction concepts: %w", err)
	}

	return correctionViewFromRecord(rec, concepts), nil
}

func correctionViewFromRecord(rec storage.CorrectionRecord, concepts []string) CorrectionView {
	return CorrectionView{
		Correction: correction.Correction{
			ID:          rec.ID,
			Original:    rec.Original,
			Replacement: rec.Replacement,
			Type:        correction.Type(rec.Type),
			Severity:    correction.Severity(rec.Severity),
			Explanation: correction.Explanation{JA: rec.ExplanationJA, EN: rec.ExplanationEN},
			Concepts:    concepts,
		},
		Status: rec.Status,
		Diff:   diff.Runes(rec.Original, rec.Replacement),
	}
}

// recentErrors formats identity's top priorities (PRD §16's heuristic
// teaching planner — internal/application/planner keeps
// storage.PriorityRepository current) as the human-readable lines the
// teacher.feedback.v2 prompt's RecentErrors range renders directly
// into the system context ("weigh these when deciding severity"). A
// learner with no priorities yet (the common case for a brand-new
// identity, or before the learner model has flagged anything) gets an
// empty slice — the prompt template already tolerates that via
// {{if .RecentErrors}}.
func (s *Service) recentErrors(ctx context.Context, identity learner.IdentityID) ([]string, error) {
	top, err := s.priorities.Top(ctx, identity, recentErrorsLimit)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(top))
	for _, p := range top {
		lines = append(lines, fmt.Sprintf("%s (%s weakness, score %.1f): %s", p.Subject, p.SubjectType, p.Score, p.Reason))
	}
	return lines, nil
}

// expressionsToEncourage formats identity's top activation candidates
// (PRD §55/§17.5's vocabulary activator — application/planner.Planner's
// ActivationCandidates, capped at activationCandidatesLimit) as the
// human-readable lines the teacher.feedback.v2 prompt's
// ExpressionsToEncourage range renders directly: "expression (reading)
// — meaning", with the parenthetical reading dropped when it's empty or
// identical to the expression itself (a pure-kana entry has nothing to
// disambiguate). An identity with no candidates yet gets an empty
// slice — the prompt template already tolerates that via
// {{if .ExpressionsToEncourage}}.
func (s *Service) expressionsToEncourage(ctx context.Context, identity learner.IdentityID) ([]string, error) {
	items, err := s.planner.ActivationCandidates(ctx, identity, activationCandidatesLimit)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if item.Reading != "" && item.Reading != item.Expression {
			lines = append(lines, fmt.Sprintf("%s (%s) — %s", item.Expression, item.Reading, item.Meaning))
		} else {
			lines = append(lines, fmt.Sprintf("%s — %s", item.Expression, item.Meaning))
		}
	}
	return lines, nil
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
