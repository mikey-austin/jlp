package postgres

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file is the enforcement mechanism for soft delete (Phase 4 Task
// D). It has no build tag on purpose: it needs no database, runs under
// plain `go test ./...` and `make test`, and is meant to fail the build
// the moment someone adds a query that can see deleted rows.
//
// The rule it enforces: a learner's delete HIDES a session, a
// vocabulary item or a lesson everywhere, forever, without any caller
// having to remember. There are more than a dozen read surfaces —
// /sessions and the session workspace, the feedback history, all four
// /vocabulary filter tabs, /lessons, the JSON API, the agent tool
// registry that feeds the AI, the A2A skills, the weekly summary, the
// MQTT bridge, the lesson generator — and every one of them reaches
// data through db/queries. Documenting "remember AND deleted_at IS
// NULL" would be re-derived wrongly by the next consumer, the way this
// project has already had to fix the same shape of bug repeatedly. So
// the predicate lives in SQL, once per query, and this test asserts it
// is there.
//
// Adding a query that touches guarded data and forgetting the
// predicate fails TestEveryQueryFiltersSoftDeleted with the query's
// name. The ONLY way past it is to add the query to softDeleteExempt
// with a written reason — which is the point: seeing deleted rows
// becomes a decision someone made and justified, not an omission.

// guardedTables are the tables whose rows a learner can make disappear.
//
// The first three own a deleted_at column of their own. The rest have
// no deleted_at and never will: each one hangs off exactly one session
// by a NOT NULL foreign key (documents.session_id,
// feedback_requests.session_id, conversations.session_id, and their own
// children through those), so "is this row deleted?" has one answer —
// "is its session deleted?" — and asking it twice in two places is how
// the two answers drift apart. Those queries reach the session with an
// EXISTS sub-select; see the header comment in db/queries/documents.sql
// for the exact spelling.
// retrieval_items is the odd one out and is listed for exactly that
// reason: it has NO foreign key to vocabulary_items — its "expression"
// subjects are the vocabulary expression as free text — so nothing
// about the schema would stop a new query there from naming a deleted
// word. Listing it here is what makes that a failing build rather than
// a leak nobody looks for. See db/queries/retrieval.sql's header.
var guardedTables = []string{
	"sessions",
	"vocabulary_items",
	"lessons",
	"retrieval_items",
	"lesson_observations",
	"documents",
	"document_versions",
	"feedback_requests",
	"corrections",
	"correction_concepts",
	"conversations",
	"conversation_turns",
	// agent_runs looks operational — it is a trace of an AI call — but
	// system/input/output hold the learner's own text verbatim and
	// /ai/agents renders it, so a deleted session leaked its writing
	// there until ListAgentRuns/GetAgentRun were filtered. Listed for
	// the same reason retrieval_items is: the leak was in a table
	// nobody thought of as content. tool_calls and agent_turns hang off
	// agent_runs and are only ever reached through it.
	"agent_runs",
	"tool_calls",
	"agent_turns",
}

// softDeleteFilter is the one predicate every guarded read applies.
// Spelled identically everywhere so this test can find it and so a
// reader grepping for it finds every site.
const softDeleteFilter = "deleted_at IS NULL"

// softDeleteExempt lists, with a reason, every query allowed to touch
// guarded data WITHOUT the filter. Three kinds of entry live here and
// nothing else should:
//
//  1. Statistics. This is the whole point of the user's choice of soft
//     delete over erasure: JLP is event-sourced, learning_events is
//     append-only, and `make rebuild-model` replays the entire stream.
//     The practice happened. Tidying a word list is not a claim that it
//     didn't, so /learner, /outcomes, the home dashboard and the weekly
//     summary must read the SAME numbers before and after a delete.
//     Adding the filter to any of these would silently rewrite a
//     learner's own history — the exact failure the design exists to
//     avoid. The line is: counted history stays, rendered text hides.
//
//  2. Writes whose ownership is already established. An INSERT reached
//     only after its parent was resolved by a query that DOES filter,
//     inside the same request or the same transaction.
//
//  3. The soft-delete machinery itself — the UPDATEs that set and clear
//     deleted_at. They must see deleted rows; that is their job, and
//     they are the "separate, explicitly-named methods" this design
//     calls for rather than a general unfiltered read.
var softDeleteExempt = map[string]string{
	// (1) Statistics — must not move when a learner deletes something.
	"CountSessions":             "statistics: the home dashboard's cumulative セッション count and the weekly summary's 'across N sessions'. The sessions happened; hiding one from a list is not a claim it never took place.",
	"CountRunesWritten":         "statistics: cumulative characters written, same reasoning as CountSessions.",
	"CountFeedbackRequests":     "statistics: cumulative reviews requested, same reasoning as CountSessions.",
	"CountCorrectionsByStatus":  "statistics: the presented/accepted/rejected totals behind /learner. Event-derived history, not content.",
	"TopErrorTypes":             "statistics: the learner's five most frequent error types. Filtering would retroactively change what they have historically got wrong.",
	"VocabFunnel":               "statistics: /learner's lookups -> produced -> produced-correctly funnel. Those productions happened; deleting the word does not un-produce it.",
	"ConceptStats":              "statistics: /grammar's per-concept encounter counts. Its sibling CorrectionsForConcept, which renders the correction TEXT, does filter — counted history stays, rendered text hides.",
	"ConceptCorrectionOutcomes": "statistics: /outcomes' did-the-correction-work measure. /outcomes must read identically before and after a delete.",
	"ConceptProducedCorrectly":  "statistics: /outcomes again. vocabulary_items is joined here only as an expression -> concept lookup table over learning_events; filtering it would drop historical production events from the report.",
	"WeeklyAssistanceRate":      "statistics: /outcomes' weekly hint/reveal assistance trend.",

	// (2) Writes whose parent was already resolved by a filtered query.
	"CreateSession":                "creates the session; there is nothing to have been deleted yet.",
	"InsertLesson":                 "creates the lesson; nothing to have been deleted yet.",
	"InsertDocumentVersion":        "runs in the same transaction as UpdateDocumentContent, immediately after it returned a row — and that query does filter. A version can only be appended to a document that just proved it is live.",
	"InsertFeedbackRequest":        "application/feedback.Service.RequestFeedback resolves the session through SessionRepository.Get (filtered) before this runs; a deleted session cannot reach it.",
	"InsertCorrection":             "written in the same transaction as the InsertFeedbackRequest above, for the request it just created.",
	"InsertCorrectionConcept":      "correction_concepts is keyed by a correction id created moments earlier in the same feedback write path.",
	"GetCorrectionConcepts":        "keyed by a correction id the caller has already resolved through GetCorrection / GetFeedbackRequest, both of which filter. Returns concept slugs only — no learner text.",
	"UpsertVocabularyItemOnLookup": "the resurrect-on-lookup path: it deliberately CLEARS deleted_at when the learner looks a deleted word up again. See its own comment in db/queries/vocabulary.sql for why that beats the alternative.",
	"InsertVocabularyItemIfAbsent": "`jlp seed`'s expression-bank insert. ON CONFLICT DO NOTHING, so a deleted row is left exactly as it is — the seed must never undo a delete.",
	"UpsertVocabularyWord":         "the bulk deck sync (POST /api/v1/words). deleted_at is absent from its SET list on purpose: if a sync resurrected words, a synced word could never be deleted at all.",
	"GetVocabularyItem":            "the client_event_id replay branch inside UpsertOnLookup — a retried POST must return what the first one returned, unchanged, including for a since-deleted word. See its own comment in db/queries/vocabulary.sql.",
	"InsertAgentRun":               "creates the run; a run cannot be started for a session RequestFeedback/the conversation tutor did not already resolve through a filtered SessionRepository.Get.",
	"FinishAgentRun":               "closes out the run this same request opened moments earlier.",
	"InsertToolCall":               "written during a run that is already in flight, scoped by a join to agent_runs.",
	"InsertAgentTurn":              "written during a run that is already in flight, scoped by a join to agent_runs.",
	"ListToolCallsForRun":          "keyed by an agent_run_id the caller has already resolved through GetAgentRun, which filters — /ai/agents/{id} 404s on a deleted session's run before these are fetched.",
	"ListAgentTurnsForRun":         "keyed by an agent_run_id the caller has already resolved through GetAgentRun, which filters — see ListToolCallsForRun.",
	"UpsertRetrievalItem":          "writes one learner's spaced-retrieval schedule. It is only ever reached for an expression that production detection just matched, and detection reads AllExpressions, which filters — so a deleted word never gets here.",
	"GetRetrievalItem":             "application/retrieval.Scheduler's read-back of the schedule it is about to update, keyed by the exact subject it was handed. Returns only counts and timestamps, never rendered to a learner; the two queries that ARE rendered (DueRetrievalItems, ListRetrievalItems) do filter.",

	// (3) The soft-delete machinery itself.
	"SoftDeleteSession":        "sets deleted_at. Idempotent by design, so it must match an already-deleted row.",
	"RestoreSession":           "clears deleted_at; it exists precisely to reach deleted rows.",
	"SoftDeleteLesson":         "sets deleted_at; see SoftDeleteSession.",
	"RestoreLesson":            "clears deleted_at; see RestoreSession.",
	"SoftDeleteVocabularyItem": "sets deleted_at; see SoftDeleteSession.",
	"RestoreVocabularyItem":    "clears deleted_at; see RestoreSession.",
}

func TestEveryQueryFiltersSoftDeleted(t *testing.T) {
	queries := loadQueries(t)
	if len(queries) == 0 {
		t.Fatal("no queries parsed out of db/queries — the parser or the path is wrong, which would make this whole guard silently vacuous")
	}

	var missing []string
	for _, q := range queries {
		touched := q.touches(guardedTables)
		if len(touched) == 0 {
			continue
		}
		if strings.Contains(q.sql, softDeleteFilter) {
			continue
		}
		if _, ok := softDeleteExempt[q.name]; ok {
			continue
		}
		missing = append(missing, q.name+" ("+q.file+", touches "+strings.Join(touched, ", ")+")")
	}

	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("query %s can see soft-deleted rows: it reads or writes guarded data but has no %q, and is not in softDeleteExempt.\n"+
			"Add %q to the query's WHERE clause, or — if it genuinely must see deleted rows — add the query to softDeleteExempt with a written reason.",
			m, softDeleteFilter, softDeleteFilter)
	}
}

// TestSoftDeleteExemptionsAreAllLive keeps the exemption list honest.
// A reason that outlives the query it excused is worse than no list at
// all: it makes the next reader believe the omission was considered.
func TestSoftDeleteExemptionsAreAllLive(t *testing.T) {
	queries := loadQueries(t)
	byName := make(map[string]sqlQuery, len(queries))
	for _, q := range queries {
		byName[q.name] = q
	}

	for name, reason := range softDeleteExempt {
		q, ok := byName[name]
		if !ok {
			t.Errorf("softDeleteExempt names %q, which no longer exists in db/queries — delete the entry", name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("softDeleteExempt[%q] has an empty reason; an exemption without a reason is an omission with extra steps", name)
		}
		if len(q.touches(guardedTables)) == 0 {
			t.Errorf("softDeleteExempt names %q, but that query no longer touches any guarded table — delete the entry", name)
		}
	}
}

type sqlQuery struct {
	name string
	file string
	sql  string // comment lines stripped
}

// touches reports which guarded tables the query's SQL references. It
// matches whole words only, so "corrections" does not match
// "correction_concepts" and vice versa.
func (q sqlQuery) touches(tables []string) []string {
	var out []string
	for _, tbl := range tables {
		if containsWord(q.sql, tbl) {
			out = append(out, tbl)
		}
	}
	return out
}

func containsWord(haystack, word string) bool {
	for i := 0; ; {
		j := strings.Index(haystack[i:], word)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(word)
		if !isIdentByte(haystack, start-1) && !isIdentByte(haystack, end) {
			return true
		}
		i = end
	}
}

func isIdentByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// loadQueries parses db/queries/*.sql into one entry per "-- name: X"
// block, with every comment line removed — so a table name or a
// "deleted_at IS NULL" written in prose can neither trip the guard nor
// satisfy it.
func loadQueries(t *testing.T) []sqlQuery {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "db", "queries")

	// WalkDir, not ReadDir: a query file tucked into a subdirectory
	// would otherwise be invisible to this guard — the one way to add a
	// leaking query without the build noticing.
	var files []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".sql") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	var out []sqlQuery
	for _, path := range files {
		e := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", e, err)
		}
		var cur *sqlQuery
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if after, ok := strings.CutPrefix(trimmed, "-- name:"); ok {
				if cur != nil {
					out = append(out, *cur)
				}
				name, _, _ := strings.Cut(strings.TrimSpace(after), " ")
				cur = &sqlQuery{name: strings.TrimSuffix(name, ":"), file: e}
				continue
			}
			if cur == nil || strings.HasPrefix(trimmed, "--") {
				continue
			}
			// Strip a TRAILING comment too, not just whole-line ones:
			// "WHERE identity_id = $1 -- deleted_at IS NULL not needed"
			// would otherwise satisfy the guard with prose. Postgres has
			// no string literal containing "--" anywhere in db/queries,
			// so cutting at the first one is safe here.
			if before, _, found := strings.Cut(line, "--"); found {
				line = before
			}
			cur.sql += line + "\n"
		}
		if cur != nil {
			out = append(out, *cur)
		}
	}
	return out
}
