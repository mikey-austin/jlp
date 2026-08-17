//go:build integration

package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Soft delete against a real database (Phase 4 Task D).
//
// internal/adapters/postgres/softdelete_guard_test.go proves every
// query CARRIES the filter. This file proves the filter WORKS, by
// building one learner's world — a session with its document, versions,
// conversation turns, feedback request and corrections; a vocabulary
// item; a lesson with a tutor observation — deleting one of each kind,
// and then driving every repository read there is.
//
// TestSoftDeleteLeakSweep is the sweep the brief asks for. It is
// deliberately a list of named surfaces rather than a handful of spot
// checks: the repository reads enumerated there are the whole of what
// the agent tool registry, the A2A skills, the weekly summary, the MQTT
// bridge, the JSON API and every HTML page can see, because none of
// them has SQL of its own.
//
// TestSoftDeleteLeavesStatisticsUnchanged is the other half, and it is
// not a formality: it is the reason the user chose "hide it, keep the
// history" over erasure. If /learner or /outcomes move when a learner
// tidies their word list, the design is wrong.

type softDeleteFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool

	sessions  *SessionRepository
	docs      *DocumentRepository
	convos    *ConversationRepository
	feedback  *FeedbackRepository
	vocab     *VocabularyRepository
	lessons   *LessonRepository
	grammar   *GrammarRepository
	stats     *AnalyticsRepository
	outcomes  *OutcomeRepository
	retrieval *RetrievalRepository
	agentRuns *AgentRunRepository
}

func newSoftDeleteFixture(t *testing.T) *softDeleteFixture {
	t.Helper()
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &softDeleteFixture{
		t: t, ctx: ctx, pool: pool,
		sessions:  NewSessionRepository(pool),
		docs:      NewDocumentRepository(pool),
		convos:    NewConversationRepository(pool),
		feedback:  NewFeedbackRepository(pool),
		vocab:     NewVocabularyRepository(pool),
		lessons:   NewLessonRepository(pool),
		grammar:   NewGrammarRepository(pool),
		stats:     NewAnalyticsRepository(pool),
		outcomes:  NewOutcomeRepository(pool),
		retrieval: NewRetrievalRepository(pool),
		agentRuns: NewAgentRunRepository(pool),
	}
}

func (f *softDeleteFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("seed %q: %v", sql, err)
	}
}

func (f *softDeleteFixture) identity(prefix string) learner.IdentityID {
	f.t.Helper()
	id := learner.IdentityID(prefix + "-" + uuid.NewString())
	if err := NewIdentityRepository(f.pool).Upsert(f.ctx, learner.Identity{ID: id, DisplayName: prefix}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// world is everything one learner accumulated: the ids a test needs to
// delete things and then look for them again.
type world struct {
	identity     learner.IdentityID
	sessionID    session.ID
	documentID   writing.DocumentID
	conversation string
	feedbackID   string
	correctionID string
	conceptSlug  string
	itemID       string
	expression   string
	lessonID     string
	agentRunID   string
}

// seedWorld builds a complete, realistic world through the repositories
// themselves wherever possible, so the rows are shaped exactly as
// production writes them.
func (f *softDeleteFixture) seedWorld(prefix string) world {
	f.t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	w := world{identity: f.identity(prefix), expression: "取り組む-" + uuid.NewString()[:8]}

	w.sessionID = session.ID(uuid.NewString())
	if err := f.sessions.Create(f.ctx, session.Session{
		ID: w.sessionID, IdentityID: w.identity, Title: "日記の練習", Purpose: "Diary",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		f.t.Fatalf("create session: %v", err)
	}

	doc, _, err := f.docs.GetOrCreateForSession(f.ctx, w.identity, w.sessionID)
	if err != nil {
		f.t.Fatalf("create document: %v", err)
	}
	w.documentID = doc.ID
	if _, err := f.docs.Save(f.ctx, w.identity, doc.ID, "今日は日記を書きました。"); err != nil {
		f.t.Fatalf("save document: %v", err)
	}

	w.conversation, _, err = f.convos.GetOrCreateForSession(f.ctx, w.identity, w.sessionID)
	if err != nil {
		f.t.Fatalf("create conversation: %v", err)
	}
	if err := f.convos.InsertTurn(f.ctx, w.identity, w.conversation, storage.ConversationTurn{
		ID: uuid.NewString(), ConversationID: w.conversation, SessionID: w.sessionID,
		Position: 1, LearnerText: "こんばんは。", Reply: "こんばんは！", CreatedAt: now,
	}); err != nil {
		f.t.Fatalf("insert turn: %v", err)
	}

	w.conceptSlug = "te-form-" + uuid.NewString()[:8]
	f.exec(`INSERT INTO grammar_concepts (slug, name, jlpt_level) VALUES ($1, 'て形', 3)`, w.conceptSlug)

	w.feedbackID = uuid.NewString()
	w.correctionID = uuid.NewString()
	if err := f.feedback.InsertFeedback(f.ctx,
		storage.FeedbackRecord{
			ID: w.feedbackID, IdentityID: w.identity, SessionID: w.sessionID, DocumentID: doc.ID,
			SelectionStart: 0, SelectionEnd: 5, SelectionText: "今日は日記を", CorrectedText: "今日は日記を",
			CreatedAt: now,
		},
		[]storage.CorrectionRecord{{
			ID: w.correctionID, Position: 0, Original: "まちがい", Replacement: "ただしい",
			Type: "grammar", Severity: "incorrect", Status: "accepted",
		}},
		map[string][]storage.ConceptTag{w.correctionID: {{Slug: w.conceptSlug, Resolved: true}}},
	); err != nil {
		f.t.Fatalf("insert feedback: %v", err)
	}

	item, _, err := f.vocab.UpsertOnLookup(f.ctx, w.identity, w.expression, "とりくむ", "to tackle", "novel", "例文", vocabulary.KindExpression, "", now)
	if err != nil {
		f.t.Fatalf("upsert vocabulary: %v", err)
	}
	w.itemID = item.ID

	w.lessonID = uuid.NewString()
	if err := f.lessons.Insert(f.ctx, storage.Lesson{
		ID: w.lessonID, IdentityID: w.identity, Plan: []byte(`{"level_summary":"s"}`), Status: "prepared", CreatedAt: now,
	}); err != nil {
		f.t.Fatalf("insert lesson: %v", err)
	}
	if _, err := f.lessons.CompleteWithObservation(f.ctx, w.identity, w.lessonID, storage.LessonObservation{
		ID: uuid.NewString(), LessonID: w.lessonID, Author: "tutor", Notes: "よくできました", Subjects: []string{w.conceptSlug}, CreatedAt: now,
	}, now); err != nil {
		f.t.Fatalf("complete lesson: %v", err)
	}

	// The spaced-retrieval schedule the word earned by being produced.
	// retrieval_items is keyed by (identity, subject_type, subject)
	// TEXT with no foreign key to vocabulary_items, so it is the one
	// place a deleted word can still surface by name.
	if err := f.retrieval.Upsert(f.ctx, storage.RetrievalItem{
		IdentityID: w.identity, SubjectType: "expression", Subject: w.expression,
		Successes: 1, LastSeen: now, DueAt: now.Add(-time.Hour), Interval: 24 * time.Hour,
	}); err != nil {
		f.t.Fatalf("upsert retrieval item: %v", err)
	}

	// The agent-run trace of the review. This looks like an operational
	// record, which is why it was nearly missed: system/input/output
	// hold the learner's own text verbatim, and /ai/agents renders it.
	w.agentRunID = uuid.NewString()
	if err := f.agentRuns.Start(f.ctx, storage.AgentRun{
		ID: w.agentRunID, IdentityID: w.identity, SessionID: &w.sessionID,
		Agent: "teacher", PromptName: "teacher", PromptVersion: "v1", Status: "running",
		StartedAt: now, System: "system prompt", Input: "今日は日記を書きました。",
	}); err != nil {
		f.t.Fatalf("start agent run: %v", err)
	}
	if err := f.agentRuns.RecordToolCall(f.ctx, w.identity, storage.ToolCall{
		ID: uuid.NewString(), AgentRunID: w.agentRunID, ToolName: "get_recent_writing",
		Arguments: `{}`, Result: `{"content":"今日は日記を書きました。"}`, CreatedAt: now,
	}); err != nil {
		f.t.Fatalf("record tool call: %v", err)
	}
	if err := f.agentRuns.Finish(f.ctx, w.identity, w.agentRunID, "completed", "", "final answer", 1, now); err != nil {
		f.t.Fatalf("finish agent run: %v", err)
	}

	// The learning events behind all of it — the history soft delete
	// exists to preserve.
	f.exec(`INSERT INTO learning_events (id, identity_id, session_id, type, subject, evidence, occurred_at)
	        VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6)`,
		uuid.NewString(), string(w.identity), string(w.sessionID), string(event.TypeCorrectionPresented), w.correctionID, now)
	f.exec(`INSERT INTO learning_events (id, identity_id, type, subject, evidence, occurred_at)
	        VALUES ($1, $2, $3, $4, '{}'::jsonb, $5)`,
		uuid.NewString(), string(w.identity), string(event.TypeVocabularyProducedCorrectly), w.expression, now.Add(time.Second))

	return w
}

// deleteAll soft-deletes one of each kind.
func (f *softDeleteFixture) deleteAll(w world) {
	f.t.Helper()
	at := time.Now().UTC()
	if err := f.sessions.SoftDelete(f.ctx, w.identity, w.sessionID, at); err != nil {
		f.t.Fatalf("soft-delete session: %v", err)
	}
	if err := f.vocab.SoftDelete(f.ctx, w.identity, w.itemID, at); err != nil {
		f.t.Fatalf("soft-delete vocabulary item: %v", err)
	}
	if err := f.lessons.SoftDelete(f.ctx, w.identity, w.lessonID, at); err != nil {
		f.t.Fatalf("soft-delete lesson: %v", err)
	}
}

// readSurface is one named repository read, plus a predicate that
// reports whether the deleted thing came back from it. Every surface
// the application layer, the HTTP layer, the JSON API, the agent tool
// registry, the A2A skills, the weekly summary and the MQTT bridge can
// see goes through one of these — none of them has SQL of its own.
type readSurface struct {
	name string
	// leaked runs the read and reports whether the deleted row appeared.
	leaked func(f *softDeleteFixture, w world) bool
}

func softDeleteReadSurfaces() []readSurface {
	return []readSurface{
		{"SessionRepository.List (/sessions, /api/v1/sessions, get_active_session)", func(f *softDeleteFixture, w world) bool {
			got, err := f.sessions.List(f.ctx, w.identity)
			if err != nil {
				f.t.Fatalf("sessions.List: %v", err)
			}
			for _, s := range got {
				if s.ID == w.sessionID {
					return true
				}
			}
			return false
		}},
		{"SessionRepository.Get (the workspace, /api/v1/sessions/{id}, get_session_context)", func(f *softDeleteFixture, w world) bool {
			_, err := f.sessions.Get(f.ctx, w.identity, w.sessionID)
			return err == nil
		}},
		{"DocumentRepository.GetOrCreateForSession (the workspace editor, get_recent_writing)", func(f *softDeleteFixture, w world) bool {
			_, _, err := f.docs.GetOrCreateForSession(f.ctx, w.identity, w.sessionID)
			return err == nil
		}},
		{"DocumentRepository.Get (POST /documents/{id} autosave)", func(f *softDeleteFixture, w world) bool {
			_, err := f.docs.Get(f.ctx, w.identity, w.documentID)
			return err == nil
		}},
		{"DocumentRepository.Save (autosave write path)", func(f *softDeleteFixture, w world) bool {
			_, err := f.docs.Save(f.ctx, w.identity, w.documentID, "a stale tab typing on")
			return err == nil
		}},
		{"DocumentRepository.ListVersions (version history)", func(f *softDeleteFixture, w world) bool {
			got, err := f.docs.ListVersions(f.ctx, w.identity, w.documentID, 10)
			if err != nil {
				f.t.Fatalf("docs.ListVersions: %v", err)
			}
			return len(got) > 0
		}},
		{"ConversationRepository.ListTurns (the conversation pane)", func(f *softDeleteFixture, w world) bool {
			got, err := f.convos.ListTurns(f.ctx, w.identity, w.conversation)
			if err != nil {
				f.t.Fatalf("convos.ListTurns: %v", err)
			}
			return len(got) > 0
		}},
		{"FeedbackRepository.ListForSession (the feedback history list)", func(f *softDeleteFixture, w world) bool {
			got, err := f.feedback.ListForSession(f.ctx, w.identity, w.sessionID)
			if err != nil {
				f.t.Fatalf("feedback.ListForSession: %v", err)
			}
			return len(got) > 0
		}},
		{"FeedbackRepository.GetFeedback (one past review)", func(f *softDeleteFixture, w world) bool {
			_, _, err := f.feedback.GetFeedback(f.ctx, w.identity, w.feedbackID)
			return err == nil
		}},
		{"FeedbackRepository.GetCorrection (correction card actions, create_anki_card)", func(f *softDeleteFixture, w world) bool {
			_, err := f.feedback.GetCorrection(f.ctx, w.identity, w.correctionID)
			return err == nil
		}},
		{"FeedbackRepository.RecentCorrections (get_correction_history, get_recent_errors, the lesson generator)", func(f *softDeleteFixture, w world) bool {
			got, err := f.feedback.RecentCorrections(f.ctx, w.identity, 50)
			if err != nil {
				f.t.Fatalf("feedback.RecentCorrections: %v", err)
			}
			for _, c := range got {
				if c.ID == w.correctionID {
					return true
				}
			}
			return false
		}},
		{"GrammarRepository.CorrectionsForConcept (/grammar/{slug})", func(f *softDeleteFixture, w world) bool {
			got, err := f.grammar.CorrectionsForConcept(f.ctx, w.identity, w.conceptSlug, 50)
			if err != nil {
				f.t.Fatalf("grammar.CorrectionsForConcept: %v", err)
			}
			for _, c := range got {
				if c.ID == w.correctionID {
					return true
				}
			}
			return false
		}},
		{"VocabularyRepository.List (all four /vocabulary tabs, search_vocabulary, get_vocabulary_history, the weekly summary)", func(f *softDeleteFixture, w world) bool {
			for _, filter := range []string{"", "looked-up", "produced", "activate"} {
				got, err := f.vocab.List(f.ctx, w.identity, filter)
				if err != nil {
					f.t.Fatalf("vocab.List(%q): %v", filter, err)
				}
				for _, item := range got {
					if item.ID == w.itemID {
						return true
					}
				}
			}
			return false
		}},
		{"VocabularyRepository.ListActivationCandidates (what the teacher agent is told to encourage)", func(f *softDeleteFixture, w world) bool {
			got, err := f.vocab.ListActivationCandidates(f.ctx, w.identity, 0)
			if err != nil {
				f.t.Fatalf("vocab.ListActivationCandidates: %v", err)
			}
			for _, item := range got {
				if item.ID == w.itemID {
					return true
				}
			}
			return false
		}},
		{"VocabularyRepository.AllExpressions (production detection, which re-schedules retrieval)", func(f *softDeleteFixture, w world) bool {
			got, err := f.vocab.AllExpressions(f.ctx, w.identity)
			if err != nil {
				f.t.Fatalf("vocab.AllExpressions: %v", err)
			}
			_, ok := got[w.expression]
			return ok
		}},
		{"VocabularyRepository.GetByExpressions (the retrieval scheduler's due queue)", func(f *softDeleteFixture, w world) bool {
			got, err := f.vocab.GetByExpressions(f.ctx, w.identity, []string{w.expression})
			if err != nil {
				f.t.Fatalf("vocab.GetByExpressions: %v", err)
			}
			return len(got) > 0
		}},
		{"AgentRunRepository.List (/ai/agents, which renders the run's input and output verbatim)", func(f *softDeleteFixture, w world) bool {
			got, err := f.agentRuns.List(f.ctx, w.identity, 100)
			if err != nil {
				f.t.Fatalf("agentRuns.List: %v", err)
			}
			for _, run := range got {
				if run.ID == w.agentRunID {
					return true
				}
			}
			return false
		}},
		{"AgentRunRepository.Get (/ai/agents/{id} — the full trace, including tool-call results carrying the document)", func(f *softDeleteFixture, w world) bool {
			_, _, _, err := f.agentRuns.Get(f.ctx, w.identity, w.agentRunID)
			return err == nil
		}},
		{"RetrievalRepository.List (/learner's 復習キュー)", func(f *softDeleteFixture, w world) bool {
			got, err := f.retrieval.List(f.ctx, w.identity, 0)
			if err != nil {
				f.t.Fatalf("retrieval.List: %v", err)
			}
			for _, it := range got {
				if it.Subject == w.expression {
					return true
				}
			}
			return false
		}},
		{"RetrievalRepository.Due (the due queue feeding feedback requests and practice)", func(f *softDeleteFixture, w world) bool {
			got, err := f.retrieval.Due(f.ctx, w.identity, time.Now().UTC(), 0)
			if err != nil {
				f.t.Fatalf("retrieval.Due: %v", err)
			}
			for _, it := range got {
				if it.Subject == w.expression {
					return true
				}
			}
			return false
		}},
		{"LessonRepository.List (/lessons)", func(f *softDeleteFixture, w world) bool {
			got, err := f.lessons.List(f.ctx, w.identity)
			if err != nil {
				f.t.Fatalf("lessons.List: %v", err)
			}
			for _, l := range got {
				if l.ID == w.lessonID {
					return true
				}
			}
			return false
		}},
		{"LessonRepository.Get (/lessons/{id})", func(f *softDeleteFixture, w world) bool {
			_, err := f.lessons.Get(f.ctx, w.identity, w.lessonID)
			return err == nil
		}},
		{"LessonRepository.Observations (the tutor's post-lesson notes)", func(f *softDeleteFixture, w world) bool {
			got, err := f.lessons.Observations(f.ctx, w.identity, w.lessonID)
			if err != nil {
				f.t.Fatalf("lessons.Observations: %v", err)
			}
			return len(got) > 0
		}},
	}
}

// TestSoftDeleteLeakSweep is the sweep: every read surface, driven,
// before and after. Each one is checked BEFORE the delete too — a
// surface that returned nothing all along would otherwise report a
// clean pass while testing nothing.
func TestSoftDeleteLeakSweep(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("sweep")

	for _, s := range softDeleteReadSurfaces() {
		if !s.leaked(f, w) {
			t.Fatalf("precondition: %s does not show the content BEFORE the delete, so this surface would prove nothing", s.name)
		}
	}

	f.deleteAll(w)

	for _, s := range softDeleteReadSurfaces() {
		if s.leaked(f, w) {
			t.Errorf("LEAK: %s still returns soft-deleted content", s.name)
		}
	}
}

// TestSoftDeleteRestoreBringsEverythingBack is the sweep run backwards:
// after a restore, every surface must show the content again. It is
// what makes "recoverable" a claim with evidence — and it also catches
// a delete that removed rows outright, which no amount of absence
// checking above could distinguish from a correct hide.
func TestSoftDeleteRestoreBringsEverythingBack(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("restore")
	f.deleteAll(w)

	if err := f.sessions.Restore(f.ctx, w.identity, w.sessionID); err != nil {
		t.Fatalf("restore session: %v", err)
	}
	if err := f.vocab.Restore(f.ctx, w.identity, w.itemID); err != nil {
		t.Fatalf("restore vocabulary item: %v", err)
	}
	if err := f.lessons.Restore(f.ctx, w.identity, w.lessonID); err != nil {
		t.Fatalf("restore lesson: %v", err)
	}

	for _, s := range softDeleteReadSurfaces() {
		if !s.leaked(f, w) {
			t.Errorf("%s did not come back after restore", s.name)
		}
	}
}

// TestSoftDeleteLeavesStatisticsUnchanged is the whole reason the user
// chose soft delete over erasure. JLP is event-sourced: learning_events
// is append-only and `make rebuild-model` replays it. The practice
// happened. Tidying a list is not a claim that it didn't, so every
// number on /learner, /outcomes, the home dashboard and the weekly
// summary must read identically before and after.
func TestSoftDeleteLeavesStatisticsUnchanged(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("stats")

	statsBefore, err := f.stats.Statistics(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("statistics: %v", err)
	}
	funnelBefore, err := f.stats.VocabFunnel(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("vocab funnel: %v", err)
	}
	outcomesBefore, err := f.outcomes.ConceptOutcomes(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("concept outcomes: %v", err)
	}
	conceptsBefore, err := f.grammar.ConceptStats(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("concept stats: %v", err)
	}

	// Sanity: the fixture must actually have moved these numbers off
	// zero, or "unchanged" would be a tautology.
	if statsBefore.SessionCount == 0 || statsBefore.CorrectionsPresented == 0 || funnelBefore.LookedUp == 0 {
		t.Fatalf("precondition: statistics are empty (%+v, %+v) — this test would pass vacuously", statsBefore, funnelBefore)
	}

	f.deleteAll(w)

	statsAfter, err := f.stats.Statistics(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("statistics after: %v", err)
	}
	if !reflect.DeepEqual(statsBefore, statsAfter) {
		t.Errorf("/learner + home statistics MOVED after a delete:\n before %+v\n  after %+v", statsBefore, statsAfter)
	}

	funnelAfter, err := f.stats.VocabFunnel(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("vocab funnel after: %v", err)
	}
	if !reflect.DeepEqual(funnelBefore, funnelAfter) {
		t.Errorf("/learner vocabulary funnel MOVED after a delete:\n before %+v\n  after %+v", funnelBefore, funnelAfter)
	}

	outcomesAfter, err := f.outcomes.ConceptOutcomes(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("concept outcomes after: %v", err)
	}
	if !reflect.DeepEqual(outcomesBefore, outcomesAfter) {
		t.Errorf("/outcomes MOVED after a delete:\n before %+v\n  after %+v", outcomesBefore, outcomesAfter)
	}

	conceptsAfter, err := f.grammar.ConceptStats(f.ctx, w.identity)
	if err != nil {
		t.Fatalf("concept stats after: %v", err)
	}
	if !reflect.DeepEqual(conceptsBefore, conceptsAfter) {
		t.Errorf("/grammar encounter counts MOVED after a delete:\n before %+v\n  after %+v", conceptsBefore, conceptsAfter)
	}

	// And the events themselves are still there, untouched: that is
	// what `make rebuild-model` will replay.
	var events int
	if err := f.pool.QueryRow(f.ctx, `SELECT COUNT(*)::int FROM learning_events WHERE identity_id = $1`, string(w.identity)).Scan(&events); err != nil {
		t.Fatalf("count learning events: %v", err)
	}
	if events != 2 {
		t.Errorf("learning_events for this identity = %d, want the 2 seeded — a delete must never touch the event log", events)
	}
}

// --- Authorization ------------------------------------------------------
//
// The user's explicit ask. Every case asserts BOTH that the call misses
// AND that the row is still there and still live: "returns ErrNotFound"
// alone would pass even if the row had been deleted anyway, which is
// precisely the bug worth catching.

func (f *softDeleteFixture) deletedAt(table, id string) (time.Time, bool) {
	f.t.Helper()
	var at *time.Time
	var found bool
	//nolint:gosec // table is a literal from the callers below, never input.
	err := f.pool.QueryRow(f.ctx, `SELECT deleted_at, true FROM `+table+` WHERE id = $1`, id).Scan(&at, &found)
	if err != nil {
		return time.Time{}, false
	}
	if at == nil {
		return time.Time{}, true
	}
	return *at, true
}

func TestCrossIdentityDeleteMissesAndLeavesTheRowIntact(t *testing.T) {
	f := newSoftDeleteFixture(t)
	owner := f.seedWorld("owner")
	attacker := f.seedWorld("attacker")

	cases := []struct {
		name  string
		table string
		id    string
		del   func() error
	}{
		{"session", "sessions", string(owner.sessionID), func() error {
			return f.sessions.SoftDelete(f.ctx, attacker.identity, owner.sessionID, time.Now().UTC())
		}},
		{"vocabulary item", "vocabulary_items", owner.itemID, func() error {
			return f.vocab.SoftDelete(f.ctx, attacker.identity, owner.itemID, time.Now().UTC())
		}},
		{"lesson", "lessons", owner.lessonID, func() error {
			return f.lessons.SoftDelete(f.ctx, attacker.identity, owner.lessonID, time.Now().UTC())
		}},
	}

	for _, tc := range cases {
		if err := tc.del(); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("%s: cross-identity delete = %v, want storage.ErrNotFound (indistinguishable from an unknown id)", tc.name, err)
		}
		at, exists := f.deletedAt(tc.table, tc.id)
		if !exists {
			t.Errorf("%s: the owner's row was DESTROYED by another identity's delete", tc.name)
			continue
		}
		if !at.IsZero() {
			t.Errorf("%s: the owner's row was marked deleted (deleted_at = %v) by another identity's delete", tc.name, at)
		}
	}

	// The owner still sees everything.
	for _, s := range softDeleteReadSurfaces() {
		if !s.leaked(f, owner) {
			t.Errorf("%s: the owner lost access after another identity's failed delete", s.name)
		}
	}
}

func TestCrossIdentityRestoreMisses(t *testing.T) {
	f := newSoftDeleteFixture(t)
	owner := f.seedWorld("owner")
	attacker := f.seedWorld("attacker")
	f.deleteAll(owner)

	if err := f.sessions.Restore(f.ctx, attacker.identity, owner.sessionID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("cross-identity session restore = %v, want storage.ErrNotFound", err)
	}
	if err := f.vocab.Restore(f.ctx, attacker.identity, owner.itemID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("cross-identity vocabulary restore = %v, want storage.ErrNotFound", err)
	}
	if err := f.lessons.Restore(f.ctx, attacker.identity, owner.lessonID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("cross-identity lesson restore = %v, want storage.ErrNotFound", err)
	}

	if at, _ := f.deletedAt("sessions", string(owner.sessionID)); at.IsZero() {
		t.Error("another identity's restore un-deleted the owner's session")
	}
}

func TestSoftDeleteIsIdempotentAndKeepsTheFirstTimestamp(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("idempotent")

	first := time.Now().UTC().Truncate(time.Microsecond)
	if err := f.sessions.SoftDelete(f.ctx, w.identity, w.sessionID, first); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := f.sessions.SoftDelete(f.ctx, w.identity, w.sessionID, first.Add(time.Hour)); err != nil {
		t.Fatalf("second delete: %v — deleting an already-deleted session must be a success", err)
	}

	at, exists := f.deletedAt("sessions", string(w.sessionID))
	if !exists {
		t.Fatal("the row is gone")
	}
	if !at.Equal(first) {
		t.Errorf("deleted_at = %v after a repeat delete, want the original %v — 'when did I delete this' must stay answerable", at, first)
	}
}

func TestSoftDeleteUnknownIDsMiss(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("unknown")
	at := time.Now().UTC()

	if err := f.sessions.SoftDelete(f.ctx, w.identity, session.ID(uuid.NewString()), at); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("unknown session delete = %v, want storage.ErrNotFound", err)
	}
	// A malformed id must miss the same way a well-formed unknown one
	// does — a distinguishable parse error would tell a caller probing
	// with junk something an unknown id would not.
	if err := f.sessions.SoftDelete(f.ctx, w.identity, session.ID("not-a-uuid"), at); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("malformed session id delete = %v, want storage.ErrNotFound", err)
	}
	if err := f.vocab.SoftDelete(f.ctx, w.identity, "not-a-uuid", at); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("malformed vocabulary id delete = %v, want storage.ErrNotFound", err)
	}
	if err := f.lessons.SoftDelete(f.ctx, w.identity, "not-a-uuid", at); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("malformed lesson id delete = %v, want storage.ErrNotFound", err)
	}
}

// GetOrCreateForSession's two "create" branches are the subtle half of
// the session cascade: once the Get filters deleted sessions, the
// fallthrough would try to INSERT a second document/conversation for a
// session that already has one and hit the UNIQUE index as a 500. Both
// inserts are guarded, and both must surface the miss as
// storage.ErrNotFound — callers branch on it to answer 404 rather than
// 500, so a raw pgx.ErrNoRows escaping the adapter would be a 500 where
// a "not found" belongs.
func TestGetOrCreateForADeletedSessionMissesCleanly(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("getorcreate")
	f.deleteAll(w)

	if _, _, err := f.docs.GetOrCreateForSession(f.ctx, w.identity, w.sessionID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("documents.GetOrCreateForSession on a deleted session = %v, want storage.ErrNotFound", err)
	}
	if _, _, err := f.convos.GetOrCreateForSession(f.ctx, w.identity, w.sessionID); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("conversations.GetOrCreateForSession on a deleted session = %v, want storage.ErrNotFound", err)
	}

	// And the guard did not leave a duplicate behind for the restore to
	// trip over.
	if err := f.sessions.Restore(f.ctx, w.identity, w.sessionID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	doc, created, err := f.docs.GetOrCreateForSession(f.ctx, w.identity, w.sessionID)
	if err != nil {
		t.Fatalf("documents.GetOrCreateForSession after restore: %v", err)
	}
	if created {
		t.Error("a second document was created for the restored session; the original should have come back")
	}
	if doc.ID != w.documentID {
		t.Errorf("restored document ID = %q, want the original %q", doc.ID, w.documentID)
	}
}

// A deleted lesson must not stay completable: an observation attached
// to something the learner can no longer see would be durably stored
// yet permanently unreachable.
func TestCompleteOnADeletedLessonMisses(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("complete")
	f.deleteAll(w)

	_, err := f.lessons.CompleteWithObservation(f.ctx, w.identity, w.lessonID, storage.LessonObservation{
		ID: uuid.NewString(), LessonID: w.lessonID, Author: "tutor", Notes: "後から", CreatedAt: time.Now().UTC(),
	}, time.Now().UTC())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("CompleteWithObservation on a deleted lesson = %v, want storage.ErrNotFound", err)
	}
}

// Looking a deleted word up again brings it back — the one thing that
// undoes a delete without Restore, and deliberately so: UNIQUE
// (identity_id, expression) means the lookup lands on the hidden row
// either way, and a lookup vanishing into a row the learner cannot
// reach is worse than one that visibly reappears. See
// db/queries/vocabulary.sql's UpsertVocabularyItemOnLookup.
func TestLookingADeletedWordUpAgainResurrectsIt(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("resurrect")

	if err := f.vocab.SoftDelete(f.ctx, w.identity, w.itemID, time.Now().UTC()); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	again, dup, err := f.vocab.UpsertOnLookup(f.ctx, w.identity, w.expression, "", "", "", "", vocabulary.KindExpression, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("re-lookup: %v", err)
	}
	if dup {
		t.Fatal("re-lookup was treated as a client-event replay")
	}
	if again.ID != w.itemID {
		t.Fatalf("re-lookup created a new row %q, want the same row %q back", again.ID, w.itemID)
	}

	got, err := f.vocab.List(f.ctx, w.identity, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, item := range got {
		if item.ID == w.itemID {
			found = true
		}
	}
	if !found {
		t.Fatal("the re-looked-up word is still hidden")
	}
}

// A bulk deck sync must NOT resurrect: if it did, a synced word could
// never be deleted at all — the next sync would always bring it back.
func TestBulkWordSyncDoesNotResurrectADeletedWord(t *testing.T) {
	f := newSoftDeleteFixture(t)
	w := f.seedWorld("sync")

	if err := f.vocab.SoftDelete(f.ctx, w.identity, w.itemID, time.Now().UTC()); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	if _, err := f.vocab.BulkUpsertWords(f.ctx, w.identity, []storage.WordInput{{
		Expression: w.expression, Reading: "とりくむ", Meaning: "to tackle", Source: "deck",
	}}, time.Now().UTC()); err != nil {
		t.Fatalf("bulk upsert: %v", err)
	}

	got, err := f.vocab.List(f.ctx, w.identity, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, item := range got {
		if item.ID == w.itemID {
			t.Fatal("a bulk deck sync resurrected a deleted word; deleting a synced word would then be impossible")
		}
	}
}
