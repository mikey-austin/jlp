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

// maxWordsPerRequest caps POST /api/v1/words's batch size (Phase 3
// Task 8's brief pins this exact number): comfortably larger than any
// legitimate single Anki-deck-sized sync, small enough that
// BulkUpsertWords' one-transaction loop stays bounded.
const maxWordsPerRequest = 1000

// ErrEmptyWordBatch is returned by IngestWords when words is empty —
// there is nothing to import.
var ErrEmptyWordBatch = errors.New("words is required and must not be empty")

// ErrTooManyWords is returned by IngestWords when the batch exceeds
// maxWordsPerRequest. Its exact text is pinned by Nihongo Daily's
// contract (Task 8 brief): callers surface err.Error() verbatim as the
// 400 response's "error" field.
var ErrTooManyWords = fmt.Errorf("too many words in one request (max %d)", maxWordsPerRequest)

// WordValidationError is returned by IngestWords for a single
// malformed word, naming the offending index (0-based, matching the
// wire request's "words" array) so a caller can point a reader-app
// author at exactly which entry is wrong.
type WordValidationError struct {
	Index int
	Msg   string
}

func (e *WordValidationError) Error() string {
	return fmt.Sprintf("words[%d]: %s", e.Index, e.Msg)
}

// importSampleSize is how many imported expressions IngestWords
// includes in its vocabulary.imported event's Evidence["sample"] —
// enough to eyeball what a batch contained without the event log
// carrying an unbounded list for a 1000-word sync.
const importSampleSize = 5

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
	item, duplicate, err := s.repo.UpsertOnLookup(ctx, identity, ev.Expression, ev.Reading, ev.Meaning, source, ev.Example, vocabulary.KindWord, ev.ClientEventID, time.Now().UTC())
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

// IngestWords is POST /api/v1/words's single write path (Phase 3 Task
// 8, Nihongo Daily's bulk vocabulary sync contract, implemented
// verbatim from /home/mikey/Workspace/nihongo-daily/doc/openapi.yaml):
// it validates every word, then upserts the whole batch in one
// transaction via storage.VocabularyRepository.BulkUpsertWords (NOT
// UpsertOnLookup — see that method's doc comment for why a sync must
// never touch Lookups/Productions/SuccessfulProductions), and records
// exactly ONE vocabulary.imported event for the batch — a per-word
// event for a 1000-word sync would flood the history the same number
// of rows the sync touches.
//
// Validation runs over the WHOLE batch before anything is written:
// words must be non-empty and at most maxWordsPerRequest long, and
// every entry's Expression/Reading/Meaning must be non-empty after
// TrimSpace and JLPTLevel must be 0..5 — the first violation found
// returns a *WordValidationError naming its 0-based index, and NO
// words from the batch are written (an all-or-nothing batch, matching
// BulkUpsertWords' one-transaction contract).
//
// A Recorder failure here is a hard failure for the same reason
// Ingest's is: by the time Record would run, the batch is already
// durably upserted, so a lost event would silently desync the event
// log from data that's visibly on the /vocabulary page.
func (s *Service) IngestWords(ctx context.Context, identity learner.IdentityID, words []storage.WordInput) (int, error) {
	if len(words) == 0 {
		return 0, ErrEmptyWordBatch
	}
	if len(words) > maxWordsPerRequest {
		return 0, ErrTooManyWords
	}

	trimmed := make([]storage.WordInput, len(words))
	for i, w := range words {
		w.Expression = strings.TrimSpace(w.Expression)
		w.Reading = strings.TrimSpace(w.Reading)
		w.Meaning = strings.TrimSpace(w.Meaning)
		if w.Expression == "" {
			return 0, &WordValidationError{Index: i, Msg: "kanji is required"}
		}
		if w.Reading == "" {
			return 0, &WordValidationError{Index: i, Msg: "reading is required"}
		}
		if w.Meaning == "" {
			return 0, &WordValidationError{Index: i, Msg: "meaning is required"}
		}
		if w.JLPTLevel < 0 || w.JLPTLevel > 5 {
			return 0, &WordValidationError{Index: i, Msg: "jlpt_level must be between 0 and 5"}
		}
		trimmed[i] = w
	}

	count, err := s.repo.BulkUpsertWords(ctx, identity, trimmed, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("vocabulary: bulk upsert words: %w", err)
	}

	source := "external"
	sample := make([]string, 0, importSampleSize)
	for i, w := range trimmed {
		if i == 0 && w.Source != "" {
			source = w.Source
		}
		if len(sample) < importSampleSize {
			sample = append(sample, w.Expression)
		}
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeVocabularyImported,
		Subject:    source,
		Evidence: map[string]any{
			"count":  count,
			"sample": sample,
		},
	}); err != nil {
		return 0, fmt.Errorf("vocabulary: record %s: %w", event.TypeVocabularyImported, err)
	}

	return count, nil
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
