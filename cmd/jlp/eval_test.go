package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
)

// TestScoreCase pins scoreCase's counting rules with hand-built
// correction.Result values — no AI involved, no file I/O. Each
// subtest isolates exactly one counter (or interaction between two)
// so a future change that breaks, say, trap-case FalsePos counting
// can't hide behind an unrelated TruePos assertion passing.
func TestScoreCase(t *testing.T) {
	tests := []struct {
		name   string
		c      Case
		result correction.Result
		want   CaseResult
	}{
		{
			name: "true_positive_single_match",
			c:    Case{ID: "c1", MustCorrect: []string{"面白いでした"}},
			result: correction.Result{Corrections: []correction.Correction{
				{Original: "面白いでした", Replacement: "面白かったです"},
			}},
			want: CaseResult{ID: "c1", TruePos: 1, FalseNeg: 0, FalsePos: 0, Pass: true},
		},
		{
			name:   "false_negative_no_matching_correction",
			c:      Case{ID: "c2", MustCorrect: []string{"存在しないテキスト"}},
			result: correction.Result{},
			want:   CaseResult{ID: "c2", TruePos: 0, FalseNeg: 1, FalsePos: 0, Pass: false},
		},
		{
			name: "false_positive_via_must_not_correct",
			c:    Case{ID: "c3", MustCorrect: []string{"A"}, MustNotCorrect: []string{"B"}},
			result: correction.Result{Corrections: []correction.Correction{
				{Original: "A"},
				{Original: "xxxBxxx"},
			}},
			want: CaseResult{ID: "c3", TruePos: 1, FalseNeg: 0, FalsePos: 1, Pass: false},
		},
		{
			name: "trap_case_any_correction_is_false_positive",
			c:    Case{ID: "trap1", MustCorrect: []string{}},
			result: correction.Result{Corrections: []correction.Correction{
				{Original: "何か", Replacement: "何かに"},
			}},
			want: CaseResult{ID: "trap1", TruePos: 0, FalseNeg: 0, FalsePos: 1, Pass: false},
		},
		{
			name:   "trap_case_no_corrections_passes",
			c:      Case{ID: "trap2", MustCorrect: nil},
			result: correction.Result{},
			want:   CaseResult{ID: "trap2", TruePos: 0, FalseNeg: 0, FalsePos: 0, Pass: true},
		},
		{
			name: "trap_case_correction_matching_must_not_correct_counts_once",
			c:    Case{ID: "trap3", MustCorrect: []string{}, MustNotCorrect: []string{"何か"}},
			result: correction.Result{Corrections: []correction.Correction{
				{Original: "何か"},
			}},
			want: CaseResult{ID: "trap3", TruePos: 0, FalseNeg: 0, FalsePos: 1, Pass: false},
		},
		{
			name: "multiple_must_correct_matched_by_one_correction",
			c:    Case{ID: "c7", MustCorrect: []string{"A", "B"}},
			result: correction.Result{Corrections: []correction.Correction{
				{Original: "AB"},
			}},
			want: CaseResult{ID: "c7", TruePos: 2, FalseNeg: 0, FalsePos: 0, Pass: true},
		},
		{
			name: "must_not_correct_does_not_flag_unrelated_correction",
			c:    Case{ID: "c8", MustCorrect: []string{"A"}, MustNotCorrect: []string{"Z"}},
			result: correction.Result{Corrections: []correction.Correction{
				{Original: "A"},
			}},
			want: CaseResult{ID: "c8", TruePos: 1, FalseNeg: 0, FalsePos: 0, Pass: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreCase(tt.c, tt.result)
			if got != tt.want {
				t.Errorf("scoreCase() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestAggregate pins the report-math edge cases directly: the "1.0
// when no positives" default for both precision and recall, a
// realistic mixed run, and an all-pass/all-trap run — without
// exercising scoreCase or any I/O.
func TestAggregate(t *testing.T) {
	t.Run("empty_corpus_defaults_to_perfect_scores", func(t *testing.T) {
		precision, recall, fpRate := aggregate(nil, nil)
		if precision != 1.0 || recall != 1.0 || fpRate != 0.0 {
			t.Errorf("aggregate() = (%v, %v, %v), want (1, 1, 0)", precision, recall, fpRate)
		}
	})

	t.Run("single_passing_trap_case", func(t *testing.T) {
		cases := []Case{{ID: "trap", MustCorrect: []string{}}}
		results := []CaseResult{{ID: "trap", TruePos: 0, FalseNeg: 0, FalsePos: 0, Pass: true}}
		precision, recall, fpRate := aggregate(cases, results)
		if precision != 1.0 || recall != 1.0 || fpRate != 0.0 {
			t.Errorf("aggregate() = (%v, %v, %v), want (1, 1, 0)", precision, recall, fpRate)
		}
	})

	t.Run("mixed_run", func(t *testing.T) {
		cases := []Case{
			{ID: "a", MustCorrect: []string{"x"}},
			{ID: "b", MustCorrect: []string{"y", "z"}},
			{ID: "trap", MustCorrect: []string{}},
		}
		results := []CaseResult{
			{ID: "a", TruePos: 1, FalseNeg: 0, FalsePos: 0, Pass: true},
			{ID: "b", TruePos: 1, FalseNeg: 1, FalsePos: 1, Pass: false},
			{ID: "trap", TruePos: 0, FalseNeg: 0, FalsePos: 1, Pass: false},
		}
		// totals: TP=2, FN=1, FP=2, trapTotal=1, trapFailed=1
		// precision = 2/(2+2) = 0.5, recall = 2/(2+1) = 0.6666..., fpRate = 1/1 = 1.0
		precision, recall, fpRate := aggregate(cases, results)
		if precision != 0.5 {
			t.Errorf("precision = %v, want 0.5", precision)
		}
		wantRecall := 2.0 / 3.0
		if recall != wantRecall {
			t.Errorf("recall = %v, want %v", recall, wantRecall)
		}
		if fpRate != 1.0 {
			t.Errorf("fpRate = %v, want 1.0", fpRate)
		}
	})
}

// TestCorpusLoadsAndMeetsMinimums exercises loadCorpus against the
// real eval/corpus/*.yaml files (not a fixture) — the corpus IS the
// deliverable here (PRD §48/§49's regression baseline), so this pins
// its structural minimums as a real test rather than trusting a
// one-off `make eval` run to notice a stripped-down or malformed
// corpus later. evalCorpusPaths is repo-root-relative (the convention
// `jlp eval` itself runs under, see cmd/jlp/eval.go's doc comment on
// evalCorpusPaths); `go test`'s working directory is this package's
// source directory instead, so paths are rebased two levels up
// (cmd/jlp -> cmd -> repo root) purely for this test.
func TestCorpusLoadsAndMeetsMinimums(t *testing.T) {
	paths := make([]string, len(evalCorpusPaths))
	for i, p := range evalCorpusPaths {
		paths[i] = filepath.Join("..", "..", p)
	}

	cases, err := loadCorpus(paths...)
	if err != nil {
		t.Fatalf("loadCorpus(%v) error = %v", paths, err)
	}

	if len(cases) < 40 {
		t.Errorf("corpus has %d cases, want >= 40", len(cases))
	}

	traps := 0
	for _, c := range cases {
		if len(c.MustCorrect) == 0 {
			traps++
		}
	}
	if traps < 10 {
		t.Errorf("corpus has %d trap cases (empty must_correct), want >= 10", traps)
	}

	// The fakeai-known patterns (internal/adapters/fakeai.go's
	// iAdjectivePastRules + を行きます particle check) must appear
	// somewhere in the corpus, or `make eval` against the offline fake
	// provider would score zero recall.
	known := []string{"面白いでした", "楽しいでした", "を行きます"}
	for _, pattern := range known {
		found := false
		for _, c := range cases {
			if strings.Contains(c.Selection, pattern) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no corpus case's selection contains fakeai-known pattern %q", pattern)
		}
	}
}

// TestRunEvalWithFakeAI exercises runEval end-to-end (real
// teacher.Agent, real fakeai — no network, no database) against a
// tiny fixed case list covering one fakeai-known pattern and one
// false-positive trap: the same shape `make eval` runs against the
// real corpus, pinned here so a change to scoreCase/aggregate/
// ReviewWriting's wiring together is caught by `go test`, not only by
// manually running `make eval` and eyeballing the report.
func TestRunEvalWithFakeAI(t *testing.T) {
	agent := teacher.New(fakeai.New())
	cases := []Case{
		{
			ID:          "known-iadj-past",
			Selection:   "昨日友達と映画を見に行って、とても面白いでした。",
			MustCorrect: []string{"面白いでした"},
		},
		{
			ID:          "trap-natural",
			Selection:   "今日は天気がいいですね。",
			MustCorrect: []string{},
		},
	}

	report, err := runEval(context.Background(), agent, cases)
	if err != nil {
		t.Fatalf("runEval() error = %v", err)
	}

	if report.Provider != "fake" {
		t.Errorf("report.Provider = %q, want %q", report.Provider, "fake")
	}
	if len(report.Cases) != 2 {
		t.Fatalf("len(report.Cases) = %d, want 2", len(report.Cases))
	}
	if report.Recall <= 0 {
		t.Errorf("report.Recall = %v, want > 0 (the fakeai-known pattern should register a true positive)", report.Recall)
	}
	if report.FPRate != 0 {
		t.Errorf("report.FPRate = %v, want 0 (fakeai returns no corrections for the unknown trap sentence)", report.FPRate)
	}

	known := report.Cases[0]
	if known.ID != "known-iadj-past" || known.TruePos != 1 || !known.Pass {
		t.Errorf("known-iadj-past result = %+v, want TruePos=1 Pass=true", known)
	}
	trap := report.Cases[1]
	if trap.ID != "trap-natural" || !trap.Pass {
		t.Errorf("trap-natural result = %+v, want Pass=true", trap)
	}
}

func TestRatio(t *testing.T) {
	if got := ratio(0, 0); got != 1.0 {
		t.Errorf("ratio(0, 0) = %v, want 1.0", got)
	}
	if got := ratio(3, 4); got != 0.75 {
		t.Errorf("ratio(3, 4) = %v, want 0.75", got)
	}
}

// TestWriteReportSameWallClockSecondDoesNotCollide pins the fix for a
// code-review finding: two `jlp eval` runs started within the same
// wall-clock second must not silently overwrite one another's report.
// evalReportTimeLayout's nanosecond resolution (vs. plain time.RFC3339's
// whole-second resolution) is what actually prevents the collision;
// this test drives writeReport directly with two timestamps that only
// differ below the second, the way two real back-to-back runs' ts
// values would.
func TestWriteReportSameWallClockSecondDoesNotCollide(t *testing.T) {
	original := evalReportsDir
	evalReportsDir = t.TempDir()
	t.Cleanup(func() { evalReportsDir = original })

	base := time.Date(2026, 8, 15, 9, 26, 55, 0, time.UTC)
	ts1 := base.Add(100 * time.Millisecond).Format(evalReportTimeLayout)
	ts2 := base.Add(900 * time.Millisecond).Format(evalReportTimeLayout)
	if ts1 == ts2 {
		t.Fatalf("test setup: ts1 == ts2 == %q, want distinct timestamps within the same second", ts1)
	}

	path1, err := writeReport(Report{Provider: "fake"}, ts1)
	if err != nil {
		t.Fatalf("writeReport() run 1 error = %v", err)
	}
	path2, err := writeReport(Report{Provider: "fake"}, ts2)
	if err != nil {
		t.Fatalf("writeReport() run 2 error = %v", err)
	}
	if path1 == path2 {
		t.Fatalf("both runs wrote to the same path %q — same-second collision not prevented", path1)
	}

	// Both files must actually exist on disk — the second write must
	// not have overwritten the first.
	if _, err := os.Stat(path1); err != nil {
		t.Errorf("path1 %q missing after run 2: %v", path1, err)
	}
	if _, err := os.Stat(path2); err != nil {
		t.Errorf("path2 %q missing: %v", path2, err)
	}

	// latestReportBefore must still recover run 1 as the report BEFORE
	// run 2's timestamp — the fixed-width format must sort lexically in
	// the same order it sorts chronologically.
	prev, err := latestReportBefore(evalReportsDir, ts2, "fake", "")
	if err != nil {
		t.Fatalf("latestReportBefore() error = %v", err)
	}
	if prev != path1 {
		t.Errorf("latestReportBefore(ts2) = %q, want %q (run 1, chronologically first)", prev, path1)
	}
}

// TestLatestReportBeforeFiltersByProviderAndModel pins the
// like-with-like baseline rule (Phase 3 Task 1, carried over from
// Phase 2's final review): the regression comparison must only ever
// consider the most recent PRIOR report with the SAME provider AND
// model as the current run — an intervening report from a different
// provider/model must be skipped over, not just ignored-but-still-
// counted-as-"a prior report exists".
func TestLatestReportBeforeFiltersByProviderAndModel(t *testing.T) {
	original := evalReportsDir
	evalReportsDir = t.TempDir()
	t.Cleanup(func() { evalReportsDir = original })

	base := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) string {
		return base.Add(offset).Format(evalReportTimeLayout)
	}

	// Oldest: anthropic/claude-x.
	oldestTS := at(0)
	if _, err := writeReport(Report{Provider: "anthropic", Model: "claude-x", Precision: 0.9}, oldestTS); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}
	// Middle: a DIFFERENT provider+model — must be skipped over when
	// comparing an anthropic/claude-x run against its history.
	middleTS := at(time.Second)
	if _, err := writeReport(Report{Provider: "ollama", Model: "llama3", Precision: 0.5}, middleTS); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}
	// Most recent matching report: anthropic/claude-x again.
	matchingTS := at(2 * time.Second)
	if _, err := writeReport(Report{Provider: "anthropic", Model: "claude-x", Precision: 0.95}, matchingTS); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}

	currentTS := at(3 * time.Second)
	got, err := latestReportBefore(evalReportsDir, currentTS, "anthropic", "claude-x")
	if err != nil {
		t.Fatalf("latestReportBefore() error = %v", err)
	}
	want := filepath.Join(evalReportsDir, matchingTS+".md")
	if got != want {
		t.Errorf("latestReportBefore() = %q, want %q (most recent matching provider+model, skipping the intervening ollama/llama3 report)", got, want)
	}

	// A provider/model pair that has never reported before has no
	// comparable baseline at all — must return "" (not an error), which
	// is what tells runEvalCommand to skip the regression check and
	// print "no comparable baseline".
	got2, err := latestReportBefore(evalReportsDir, currentTS, "openai", "gpt-4")
	if err != nil {
		t.Fatalf("latestReportBefore() error = %v", err)
	}
	if got2 != "" {
		t.Errorf("latestReportBefore() for an unseen provider/model = %q, want \"\" (no comparable baseline)", got2)
	}
}

// TestRunEvalCommandIgnoresMismatchedProviderBaseline drives
// runEvalCommand end-to-end (the exact function `jlp eval` calls) with
// a prior report seeded from a DIFFERENT provider whose scores are far
// better than the fake provider will ever achieve against the real
// corpus. If the regression check incorrectly compared against it
// anyway, this run would fail with a false regression; asserting a nil
// error pins that it doesn't.
func TestRunEvalCommandIgnoresMismatchedProviderBaseline(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))

	original := evalReportsDir
	evalReportsDir = t.TempDir()
	t.Cleanup(func() { evalReportsDir = original })

	priorTS := time.Now().UTC().Add(-time.Hour).Format(evalReportTimeLayout)
	if _, err := writeReport(Report{Provider: "anthropic", Model: "claude-x", Precision: 1.0, Recall: 1.0}, priorTS); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}

	cfg := config.Config{AI: config.AI{Provider: "fake"}}
	if err := runEvalCommand(context.Background(), cfg); err != nil {
		t.Fatalf("runEvalCommand() error = %v, want nil (a mismatched-provider baseline must not trigger a false regression)", err)
	}
}

// TestRunEvalCommandTwiceShowsDeltas drives runEvalCommand — the exact
// function main.go's `case "eval"` calls, i.e. `jlp eval`/`make eval`
// itself, not a stand-in for it — twice in a row against the real
// corpus, using the fake provider so it stays fully offline.
// evalCorpusPaths is repo-root-relative, so this test rebases its
// working directory there for its duration (`go test` otherwise runs
// from this package's own source directory).
//
// evalReportsDir is redirected to a t.TempDir() rather than left at
// its real "eval/reports": this test's fake-provider runs score a
// deliberately low recall (the fake provider only knows 3 of the
// corpus's ~30 non-trap patterns — see evalCorpusPaths' doc comment),
// and a report left behind in the REAL eval/reports/ would become the
// "previous report" a later genuine `make eval` (anthropic/ollama)
// compares itself against — corrupting that regression check with a
// fake-provider baseline it was never meant to be judged against.
//
// A second run against an unchanged corpus and a deterministic
// provider must reproduce identical scores — this pins that "no
// change" is exactly what the regression gate sees as "no change"
// (zero deltas, no false regression trip), the other half of the
// regression check TestRunEvalWithFakeAI doesn't exercise (that test
// never touches report files or the previous-run comparison at all).
func TestRunEvalCommandTwiceShowsDeltas(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))

	original := evalReportsDir
	evalReportsDir = t.TempDir()
	t.Cleanup(func() { evalReportsDir = original })

	cfg := config.Config{AI: config.AI{Provider: "fake"}}
	ctx := context.Background()

	fmt.Println("=== jlp eval — run 1 ===")
	if err := runEvalCommand(ctx, cfg); err != nil {
		t.Fatalf("runEvalCommand() run 1 error = %v", err)
	}
	report1 := latestReportFile(t)
	summary1, err := parseReportSummary(report1)
	if err != nil {
		t.Fatalf("parseReportSummary(%s) error = %v", report1, err)
	}
	t.Logf("run 1 report: %s", report1)
	t.Logf("run 1 summary: %+v", summary1)

	// RFC3339 filenames are second-granularity; wait out the second so
	// the two runs land in genuinely distinct report files rather than
	// one silently overwriting the other.
	time.Sleep(1100 * time.Millisecond)

	fmt.Println("=== jlp eval — run 2 ===")
	if err := runEvalCommand(ctx, cfg); err != nil {
		t.Fatalf("runEvalCommand() run 2 error = %v", err)
	}
	report2 := latestReportFile(t)
	if report2 == report1 {
		t.Fatalf("run 2 produced the same report path as run 1: %s", report2)
	}
	summary2, err := parseReportSummary(report2)
	if err != nil {
		t.Fatalf("parseReportSummary(%s) error = %v", report2, err)
	}
	t.Logf("run 2 report: %s", report2)
	t.Logf("run 2 summary: %+v", summary2)

	if summary2.Precision != summary1.Precision || summary2.Recall != summary1.Recall || summary2.FPRate != summary1.FPRate {
		t.Errorf("run 2 scores drifted from run 1 against an unchanged corpus + deterministic provider: run1=%+v run2=%+v", summary1, summary2)
	}
}

// latestReportFile returns the most recently created *.md file in
// evalReportsDir — the report runEvalCommand just wrote, since
// runEvalCommand itself only returns an error, not the path (matching
// every other cmd/jlp subcommand's error-only return convention).
func latestReportFile(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(evalReportsDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%s): %v", evalReportsDir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no report files found in %s", evalReportsDir)
	}
	sort.Strings(names)
	return filepath.Join(evalReportsDir, names[len(names)-1])
}
