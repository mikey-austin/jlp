// Package planner implements the heuristic teaching planner (PRD §16):
// turning the learner model's Observation rows into a ranked,
// explainable Priority list — what the Teacher agent should weigh most
// heavily right now, and why. Recompute is triggered by
// internal/application/learnermodel's Updater after every observation
// change and at the end of a Rebuild (see that package's SetPlanner),
// and its output feeds internal/application/feedback's RecentErrors.
//
// This package imports only ports/storage and domain — never
// application/learnermodel — so that package can import THIS one (to
// call Recompute) without an import cycle.
package planner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	// persistenceDivisor and persistenceCap implement
	// persistence = min(occurrences/3, 3.0).
	persistenceDivisor = 3.0
	persistenceCap     = 3.0
	// recencyWindow is the trailing window recency counts distinct
	// qualifying occurrences over — 7 days per PRD §16, independent of
	// the learner model's own 30-day weakness-detection window.
	recencyWindow = 7 * 24 * time.Hour
	// emergingPersistenceFactor: an emerging observation's score is
	// 0.5 * persistence, with recency and value dropped entirely — it
	// stays on the radar but is deliberately deprioritized versus an
	// active weakness with the same occurrence history.
	emergingPersistenceFactor = 0.5
)

// Planner turns Observation rows into a ranked Priority list. It holds
// no state beyond its collaborators: every Recompute is a fresh,
// independent fold over the identity's current observations and event
// history.
type Planner struct {
	obs     storage.ObservationRepository
	events  storage.LearningEventRepository
	grammar storage.GrammarRepository
	prios   storage.PriorityRepository
	vocab   storage.VocabularyRepository
	clock   func() time.Time
}

// NewPlanner builds a Planner. clock is injectable so tests control
// what "now" means for the recency window and each Priority's
// UpdatedAt — mirroring learnermodel.NewUpdater's injectable clock.
// vocab backs ActivationCandidates below (PRD §55/§17.5's vocabulary
// activator) — a second, independent responsibility this package took
// on in Task 7 alongside Recompute's priority scoring; it imports only
// ports/storage here too, so the package doc comment's no-cycle
// argument still holds.
func NewPlanner(obs storage.ObservationRepository, events storage.LearningEventRepository, grammar storage.GrammarRepository, prios storage.PriorityRepository, vocab storage.VocabularyRepository, clock func() time.Time) *Planner {
	return &Planner{obs: obs, events: events, grammar: grammar, prios: prios, vocab: vocab, clock: clock}
}

// ActivationCandidates returns up to limit of identity's vocabulary
// items ripe for active encouragement in the Teacher prompt (PRD
// §55/§17.5), ranked by Lookups DESC (the strongest available signal
// that an expression is "well known but dormant"): a thin passthrough
// to storage.VocabularyRepository.ListActivationCandidates, which does
// the actual filtering/ranking/capping in SQL — see that method's doc
// comment for the exact condition and why the ORDER BY/LIMIT live
// there rather than here. limit <= 0 means no cap — every matching item
// is returned, still ranked.
//
// Deliberately NOT gated behind Recompute: unlike the priority list
// (recomputed only when the learner model's observations change — see
// application/learnermodel's SetPlanner), this is a live query over
// vocabulary_items with no derived/cached state of its own, so calling
// it on every feedback request (see application/feedback.Service) costs
// one indexed SELECT, not a rebuild.
func (p *Planner) ActivationCandidates(ctx context.Context, identity learner.IdentityID, limit int) ([]vocabulary.Item, error) {
	items, err := p.vocab.ListActivationCandidates(ctx, identity, limit)
	if err != nil {
		return nil, fmt.Errorf("planner: list activation candidates: %w", err)
	}
	return items, nil
}

// TopConcept returns identity's single highest-scoring priority when —
// and only when — it's a concept-type priority: reads
// PriorityRepository.Top(identity, 1) and, if that lone highest-scoring
// row exists AND its SubjectType is "concept", resolves it to a
// grammar.Concept via GrammarRepository. application/practice.Service.
// Start uses this to decide what to drill: a learner whose top priority
// is a live grammar weakness gets drilled on exactly that concept.
//
// ok is false — with no error — in every case Start should fall back to
// a random catalog concept instead: no priorities at all, a top
// priority that's correction-type rather than concept, or a concept
// slug the catalog no longer resolves (storage.ErrNotFound from
// GetConcept — the same stale/hallucinated-slug possibility
// scoreObservation's value() already tolerates elsewhere in this file).
// Any other GetConcept or Top failure propagates as an error.
func (p *Planner) TopConcept(ctx context.Context, identity learner.IdentityID) (grammar.Concept, bool, error) {
	top, err := p.prios.Top(ctx, identity, 1)
	if err != nil {
		return grammar.Concept{}, false, fmt.Errorf("planner: top priority: %w", err)
	}
	if len(top) == 0 || top[0].SubjectType != string(learnermodel.SubjectConcept) {
		return grammar.Concept{}, false, nil
	}

	concept, err := p.grammar.GetConcept(ctx, top[0].Subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return grammar.Concept{}, false, nil
		}
		return grammar.Concept{}, false, fmt.Errorf("planner: get concept %q: %w", top[0].Subject, err)
	}
	return concept, true, nil
}

// Recompute rebuilds identity's whole priority list from its current
// observations: score every observation (see scoreObservation), sort
// score DESC, and atomically replace whatever ReplaceAll previously
// held — including clearing it to empty when identity has no
// observations at all, so a resolved learner's stale priorities don't
// linger.
func (p *Planner) Recompute(ctx context.Context, identity learner.IdentityID) error {
	observations, err := p.obs.List(ctx, identity)
	if err != nil {
		return fmt.Errorf("planner: list observations: %w", err)
	}
	all, err := p.events.ListAll(ctx, identity)
	if err != nil {
		return fmt.Errorf("planner: list events: %w", err)
	}
	now := p.clock()

	priorities := make([]storage.Priority, 0, len(observations))
	for _, o := range observations {
		pr, err := p.scoreObservation(ctx, identity, o, all, now)
		if err != nil {
			return fmt.Errorf("planner: score %s/%s: %w", o.SubjectType, o.Subject, err)
		}
		priorities = append(priorities, pr)
	}

	sort.SliceStable(priorities, func(i, j int) bool { return priorities[i].Score > priorities[j].Score })

	if err := p.prios.ReplaceAll(ctx, identity, priorities); err != nil {
		return fmt.Errorf("planner: replace priorities: %w", err)
	}
	return nil
}

// scoreObservation implements PRD §16's scoring formula:
//
//	weakness: score = persistence * recency * value
//	emerging: score = 0.5 * persistence (recency/value dropped)
//
// persistence = min(occurrences/3, 3.0), occurrences read from the
// observation's own Evidence["count"] (the 30-day weakness-detection
// count learnermodel already computed — not recomputed here). recency
// = (distinct qualifying occurrences in the trailing 7 days, from the
// event store) + 1. value is 1.0 for a correction-type subject, or
// 1.0 + 0.25*jlptWeight(concept) for a concept subject.
func (p *Planner) scoreObservation(ctx context.Context, identity learner.IdentityID, o learnermodel.Observation, all []event.LearningEvent, now time.Time) (storage.Priority, error) {
	occurrences := evidenceCount(o)
	persistence := float64(occurrences) / persistenceDivisor
	if persistence > persistenceCap {
		persistence = persistenceCap
	}

	var score float64
	var reason string
	switch o.Kind {
	case learnermodel.KindEmerging:
		score = emergingPersistenceFactor * persistence
		reason = fmt.Sprintf("emerging (recovering): %d occurrences in 30d (persistence %.2f), deprioritized while quiet", occurrences, persistence)
	default: // learnermodel.KindWeakness
		recentCount := countOccurrences(all, o.SubjectType, o.Subject, now, recencyWindow)
		recency := float64(recentCount + 1)
		value, valueDesc, err := p.value(ctx, o)
		if err != nil {
			return storage.Priority{}, err
		}
		score = persistence * recency * value
		reason = fmt.Sprintf("recurring weakness: %d occurrences in 30d (persistence %.2f), %d in last 7d (recency %.0f), %s",
			occurrences, persistence, recentCount, recency, valueDesc)
	}

	return storage.Priority{
		IdentityID:  identity,
		SubjectType: string(o.SubjectType),
		Subject:     o.Subject,
		Score:       score,
		Reason:      reason,
		UpdatedAt:   now,
	}, nil
}

// value implements the value half of the weakness formula: a
// correction-type subject has no JLPT level to weigh, so it's always
// 1.0. A concept subject looks up its JLPTLevel via GrammarRepository;
// a slug the catalog doesn't (or no longer) know about — GetConcept's
// storage.ErrNotFound — falls back to the same weight-0 band an
// N1/N2 concept gets, rather than failing Recompute outright.
func (p *Planner) value(ctx context.Context, o learnermodel.Observation) (float64, string, error) {
	switch o.SubjectType {
	case learnermodel.SubjectCorrectionType:
		return 1.0, "value 1.00 (correction-type)", nil
	case learnermodel.SubjectWord:
		// No JLPT level to weigh — vocabulary_items carries one, but a
		// word's difficulty for THIS learner is already expressed by how
		// often they fail it, which is the persistence term. Weighting it
		// twice would just amplify the same signal.
		return 1.0, "value 1.00 (word)", nil
	}

	concept, err := p.grammar.GetConcept(ctx, o.Subject)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 1.0, "value 1.00 (concept not in catalog)", nil
		}
		return 0, "", fmt.Errorf("get concept %q: %w", o.Subject, err)
	}

	weight := jlptWeight(concept.JLPTLevel)
	value := 1.0 + 0.25*float64(weight)
	return value, fmt.Sprintf("value %.2f (N%d concept, weight %d)", value, concept.JLPTLevel, weight), nil
}

// jlptWeight is the MVP JLPT-proximity heuristic: weight 2 for N4/N5
// (beginner) concepts, 1 for N3, 0 for N1/N2 and anything else — the
// active-band approximation the brief specifies in place of a real
// per-learner level model.
func jlptWeight(level int) int {
	switch level {
	case 4, 5:
		return 2
	case 3:
		return 1
	default:
		return 0
	}
}

// evidenceCount extracts Observation.Evidence["count"] as an int.
// learnermodel's live Updater stores it as a Go int, but a round trip
// through jsonb (postgres -> map[string]any) decodes JSON numbers as
// float64 — both are handled so callers never care which path produced
// the observation.
func evidenceCount(o learnermodel.Observation) int {
	switch v := o.Evidence["count"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

// --- event classification, duplicated (not imported) from
// internal/application/learnermodel/consumers.go's unexported
// classify/dedupKey/qualifyingSeverity/countOccurrences. That package
// cannot be imported here without creating a cycle (its Updater must
// import planner to call Recompute — see the package doc comment), and
// the source functions are unexported besides. Any change to the
// controller-ruling dedup semantics there (distinct
// evidence.correction_id per subject) must be mirrored here. ---

// classify reports the (SubjectType, Subject) an event contributes to,
// or ok=false for an event type/shape planner doesn't score.
func classify(ev event.LearningEvent) (subjectType learnermodel.SubjectType, subject string, ok bool) {
	switch ev.Type {
	case event.TypeCorrectionPresented:
		t, _ := ev.Evidence["type"].(string)
		if t == "" {
			return "", "", false
		}
		return learnermodel.SubjectCorrectionType, t, true
	case event.TypeGrammarConceptEncountered:
		if ev.Subject == "" {
			return "", "", false
		}
		return learnermodel.SubjectConcept, ev.Subject, true
	case event.TypeQuizAnswered:
		// Recency has to see drills too, or a weakness the learner is
		// actively failing in 練習 scores as though nothing had happened
		// for weeks — the observation would exist and rank last.
		ref, _ := ev.Evidence["subject_ref"].(string)
		if ref == "" {
			return "", "", false
		}
		switch st, _ := ev.Evidence["subject_type"].(string); st {
		case "concept":
			return learnermodel.SubjectConcept, ref, true
		case "word":
			return learnermodel.SubjectWord, ref, true
		}
		return "", "", false
	default:
		return "", "", false
	}
}

// qualifies mirrors learnermodel's rule so recency and weakness
// detection agree about what counts: a correction qualifies on severity
// ("incorrect"/"unnatural" only), a drill qualifies when it was answered
// WRONG. A drill with no severity field would otherwise be filtered out
// silently, and the classify branch above would score nothing.
func qualifies(ev event.LearningEvent) bool {
	if ev.Type == event.TypeQuizAnswered {
		correct, _ := ev.Evidence["correct"].(bool)
		return !correct
	}
	sev, _ := ev.Evidence["severity"].(string)
	return qualifyingSeverity(sev)
}

// qualifyingSeverity mirrors learnermodel's rule: only "incorrect" and
// "unnatural" severities count toward recency, same as they do toward
// the underlying weakness detection.
func qualifyingSeverity(sev string) bool {
	return sev == "incorrect" || sev == "unnatural"
}

// dedupKey is "one occurrence" for recency counting purposes, same
// controller-ruling dedup learnermodel's countOccurrences uses:
// correction.presented events dedup on their own Subject (the
// correction ID); grammar.concept.encountered events dedup on
// Evidence["correction_id"].
func dedupKey(ev event.LearningEvent) string {
	if ev.Type == event.TypeCorrectionPresented || ev.Type == event.TypeQuizAnswered {
		// The exercise id for a drill: one drill is one occurrence, and
		// answering the same exercise twice is one piece of evidence
		// about the subject rather than two. Same rule learnermodel uses.
		return ev.Subject
	}
	if cid, ok := ev.Evidence["correction_id"].(string); ok && cid != "" {
		return cid
	}
	return ev.ID
}

// countOccurrences returns the number of distinct (per dedupKey)
// qualifying occurrences of (subjectType, subject) among all, within
// window trailing back from now (inclusive, never counting events after
// now).
func countOccurrences(all []event.LearningEvent, subjectType learnermodel.SubjectType, subject string, now time.Time, window time.Duration) int {
	seen := map[string]struct{}{}
	for _, e := range all {
		st, subj, ok := classify(e)
		if !ok || st != subjectType || subj != subject {
			continue
		}
		if !qualifies(e) {
			continue
		}
		if e.OccurredAt.After(now) || now.Sub(e.OccurredAt) > window {
			continue
		}
		seen[dedupKey(e)] = struct{}{}
	}
	return len(seen)
}
