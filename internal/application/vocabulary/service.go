// Package vocabulary is the application-layer entry point for the
// learner's personal vocabulary (PRD §12): Ingest is the single write
// path for POST /api/v1/vocabulary/events (internal/adapters/http/api.go),
// and DetectProduction is called from internal/application/feedback's
// RequestFeedback to notice when a previously looked-up expression
// shows up, produced, in the learner's own writing.
package vocabulary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// minExpressionRunes is DetectProduction's floor for candidate
// expressions: a single-rune "expression" (a stray particle or kana
// entry) would match as a substring of nearly any sentence, so it's
// excluded rather than producing a constant stream of noise
// productions.
const minExpressionRunes = 2

// typeLookup is the only IngestEvent.Type PRD §12's ingestion API
// currently supports.
const typeLookup = "vocabulary.lookup"

// ErrUnsupportedType is returned by Ingest for any IngestEvent.Type
// other than "vocabulary.lookup".
var ErrUnsupportedType = errors.New("vocabulary: unsupported ingest event type")

// ErrEmptyExpression is returned by Ingest when Expression is blank —
// there is nothing to look up, track, or later detect a production of.
var ErrEmptyExpression = errors.New("vocabulary: expression is required")

type Service struct {
	repo storage.VocabularyRepository
	rec  *learning.Recorder
}

func NewService(repo storage.VocabularyRepository, rec *learning.Recorder) *Service {
	return &Service{repo: repo, rec: rec}
}

// IngestEvent is the wire shape of POST /api/v1/vocabulary/events (PRD
// §12). Source.Type/Title are combined by Ingest into the Item's free-text
// Source field (e.g. Type "novel" + Title "コンビニ人間" →
// "novel: コンビニ人間"); Example is carried through to the recorded
// vocabulary_events row for audit but has no column of its own on Item.
type IngestEvent struct {
	Type       string `json:"type"`
	Expression string `json:"expression"`
	Reading    string `json:"reading"`
	Meaning    string `json:"definition"`
	Example    string `json:"example"`
	Source     struct {
		Type  string `json:"type"`
		Title string `json:"title"`
	} `json:"source"`
	ClientEventID string `json:"client_event_id"`
}

// Ingest records one vocabulary lookup: upserts the item (see
// storage.VocabularyRepository.UpsertOnLookup for the exact
// create-vs-increment and idempotency semantics), then records a
// vocabulary.looked-up learning event — skipped when UpsertOnLookup
// reports the call as a duplicate (a retried client_event_id), since
// nothing new actually happened.
//
// A Recorder failure here is a hard failure, like feedback.Service's
// events (see that package's RequestFeedback doc comment): by the time
// Record would run, the item is already durably upserted, so a lost
// event would silently desync the event-log history from data that
// visibly exists on the /vocabulary page.
func (s *Service) Ingest(ctx context.Context, identity learner.IdentityID, ev IngestEvent) (vocabulary.Item, error) {
	if ev.Type != typeLookup {
		return vocabulary.Item{}, fmt.Errorf("%w: %q", ErrUnsupportedType, ev.Type)
	}
	if strings.TrimSpace(ev.Expression) == "" {
		return vocabulary.Item{}, ErrEmptyExpression
	}

	source := formatSource(ev.Source.Type, ev.Source.Title)
	item, duplicate, err := s.repo.UpsertOnLookup(ctx, identity, ev.Expression, ev.Reading, ev.Meaning, source, vocabulary.KindWord, ev.ClientEventID, time.Now().UTC())
	if err != nil {
		return vocabulary.Item{}, fmt.Errorf("vocabulary: upsert on lookup: %w", err)
	}
	if duplicate {
		return item, nil
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeVocabularyLookedUp,
		Subject:    ev.Expression,
		Evidence: map[string]any{
			"source_type":  ev.Source.Type,
			"source_title": ev.Source.Title,
		},
	}); err != nil {
		return vocabulary.Item{}, fmt.Errorf("vocabulary: record %s: %w", event.TypeVocabularyLookedUp, err)
	}

	return item, nil
}

// List returns identity's vocabulary items narrowed by filter — a
// thin pass-through to storage.VocabularyRepository.List, kept on
// Service (rather than exposing the repository directly to httpx) so
// the /vocabulary page and Ingest above share one application-layer
// entry point, the same "HTML and JSON share one application layer"
// principle internal/adapters/http/api.go's package doc states.
func (s *Service) List(ctx context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error) {
	return s.repo.List(ctx, identity, filter)
}

// formatSource combines an IngestEvent's Source.Type/Title into an
// Item's free-text Source field: "novel: コンビニ人間" when both are
// given, just whichever one when only one is, "" when neither is.
func formatSource(sourceType, title string) string {
	switch {
	case sourceType != "" && title != "":
		return sourceType + ": " + title
	case sourceType != "":
		return sourceType
	default:
		return title
	}
}

// DetectProduction scans text (the learner's own writing, as
// submitted — before any correction is applied) for every one of
// identity's vocabulary expressions at least minExpressionRunes runes
// long, and records a production for each occurrence found.
//
// "successful" is computed the way the brief pins, deliberately kept
// simple rather than doing real span-offset math: an occurrence is
// successful unless the expression string substring-overlaps ANY
// entry of correctedSpans (each entry is one presented correction's
// Original string) — overlap meaning either contains the other,
// checked with plain strings.Contains in both directions. This can
// over- or under-fire relative to true character-offset overlap in
// pathological cases (e.g. the same short expression appearing twice,
// only one occurrence actually touched by a correction), but for the
// common case — a correction's Original either contains the looked-up
// expression, or the expression contains a shorter corrected span —
// it matches the learner-visible reality: was this word touched by
// feedback, or not. correctedSpans entries that are the empty string
// are skipped (an empty Original substring-matches everything and
// would otherwise mark every production unsuccessful).
//
// Called from feedback.Service.RequestFeedback's end, non-fatal on
// error (see that call site's comment for why): production detection
// enriches the vocabulary catalog, it isn't part of the review's core
// history the way feedback.requested/correction.presented are.
func (s *Service) DetectProduction(ctx context.Context, identity learner.IdentityID, sessionID session.ID, text string, correctedSpans []string) error {
	expressions, err := s.repo.AllExpressions(ctx, identity)
	if err != nil {
		return fmt.Errorf("vocabulary: list expressions: %w", err)
	}

	now := time.Now().UTC()
	for expression, itemID := range expressions {
		if len([]rune(expression)) < minExpressionRunes {
			continue
		}
		if !strings.Contains(text, expression) {
			continue
		}

		successful := true
		for _, span := range correctedSpans {
			if span == "" {
				continue
			}
			if strings.Contains(span, expression) || strings.Contains(expression, span) {
				successful = false
				break
			}
		}

		if err := s.repo.RecordProduction(ctx, identity, itemID, successful, now); err != nil {
			return fmt.Errorf("vocabulary: record production for %q: %w", expression, err)
		}

		evType := event.TypeVocabularyProduced
		if successful {
			evType = event.TypeVocabularyProducedCorrectly
		}
		if err := s.rec.Record(ctx, event.LearningEvent{
			IdentityID: identity,
			SessionID:  &sessionID,
			Type:       evType,
			Subject:    expression,
			Evidence:   map[string]any{"item_id": itemID},
		}); err != nil {
			return fmt.Errorf("vocabulary: record %s: %w", evType, err)
		}
	}
	return nil
}
