package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// `jlp eval` (wrapped by `make eval`, PRD §48/§49): runs the curated
// Japanese-correction eval corpus (eval/corpus/*.yaml) through the
// CONFIGURED AI provider chain — the same buildAIGenerator router every
// other agent uses, respecting APP_AI_PROVIDER/APP_AI_ROUTES — and
// writes a timestamped Markdown report under eval/reports/, comparing
// it against the most recent prior report and exiting 1 if precision
// or recall regressed by more than evalRegressionThreshold. This is
// the corpus-level counterpart to the per-response AI quality
// dashboard (Task 10): where /ai tracks live learner-facing calls,
// `make eval` is a repeatable, offline-friendly benchmark run against
// a fixed, hand-authored set of known-good/known-bad Japanese
// sentences — the regression baseline PRD §49 asks for when comparing
// providers or prompt versions.

const (
	// evalPromptVersion mirrors internal/agent/teacher's own unexported
	// promptVersion constant (currently "v3" for every teacher mode —
	// see that package's doc comment on why). It isn't imported because
	// teacher.go deliberately exports no version constant of its own
	// (Task 13's brief scopes this task's changes to cmd/jlp + Makefile
	// + .gitignore, not the teacher package) — so this is a mirror, not
	// a shared source of truth, and must be kept in sync by hand if
	// agent/teacher ever bumps its prompt version.
	evalPromptVersion = "v3"

	// evalRegressionThreshold is PRD §49's regression gate: a drop of
	// MORE than this many points (5%) in precision or recall versus the
	// most recent prior report fails the run (os.Exit(1) from
	// cmd/jlp/main.go's generic error handling), the same way a CI
	// coverage or lint regression would. A drop of exactly the
	// threshold is tolerated (strict >, not >=) — this is a smoke
	// alarm for real regressions, not a zero-tolerance ratchet that
	// would false-positive on ordinary AI response variance.
	evalRegressionThreshold = 0.05

	// reportPrecision is how many decimal places Precision/Recall/
	// FPRate are rounded to before being printed, persisted to a
	// report's yaml block, and compared against a prior report — one
	// fixed precision used consistently at every one of those steps, so
	// a rounded-then-reparsed previous score and a freshly computed
	// current score are always compared like-for-like (see
	// runEvalCommand's roundTo calls).
	reportPrecision = 4

	// evalReportTimeLayout is RFC3339 with a FIXED nine-digit
	// fractional-second suffix — "0" placeholders in Go's reference
	// layout always pad to that width, unlike the "9" placeholders
	// time.RFC3339Nano uses, which trim trailing zeros (and drop the
	// fractional part entirely when it's exactly zero). Still valid
	// RFC3339 (its grammar allows arbitrary-precision fractional
	// seconds), but at nanosecond rather than whole-second resolution:
	// two `jlp eval` runs started within the same wall-clock second
	// (e.g. a script or CI retry invoking it twice back to back) get
	// distinct report filenames instead of the second one silently
	// overwriting the first run's report. The fixed width also matters
	// for latestReportBefore's plain lexical-string comparison below —
	// a variable-width format like RFC3339Nano's would sort out of
	// chronological order whenever two timestamps trimmed a different
	// number of trailing zeros.
	evalReportTimeLayout = "2006-01-02T15:04:05.000000000Z"

	// evalIdentity/evalSessionID tag every eval AI request with a
	// fixed, obviously-synthetic identity/session — eval never reads or
	// writes real learner data, so these exist purely to give
	// ai.StructuredRequest something to carry (observability.NewAIObserver's
	// audit record wants an IdentityID either way) without colliding
	// with any real identity/session ID a live deployment might have.
	evalIdentity  = learner.IdentityID("eval-corpus")
	evalSessionID = session.ID("eval-corpus")
)

// evalCorpusPaths lists the corpus files `jlp eval` loads, in the
// order their cases appear in the combined report. Paths are relative
// to the process's working directory — the same repo-root-relative
// convention cmd/jlp/seed.go's seedGrammarCatalogPath uses, true both
// inside the tools container (WORKDIR /src) and run directly from a
// checkout.
var evalCorpusPaths = []string{
	"eval/corpus/grammar-basics.yaml",
	"eval/corpus/naturalness.yaml",
	"eval/corpus/false-positive-traps.yaml",
}

// evalReportsDir holds one Markdown file per `jlp eval` run,
// timestamped (see evalReportTimeLayout) so filenames sort
// chronologically and effectively never collide, even across runs
// started within the same wall-clock second. Gitignored (see
// .gitignore) — reports are a local benchmarking artifact, not
// something committed to the repo. A var, not a const: eval_test.go's
// TestRunEvalCommandTwiceShowsDeltas redirects it to a scratch
// directory for its duration, so its fake-provider test runs never
// leave a report behind for a later REAL `make eval` run's regression
// check to mistakenly compare itself against.
var evalReportsDir = "eval/reports"

// Case is one eval corpus entry (see eval/corpus/*.yaml): a Japanese
// selection the teacher agent reviews, plus the substrings its
// response is graded against. MustCorrect/MustNotCorrect are matched
// against correction.Correction.Original by substring (scoreCase), not
// exact equality — the corpus records the minimal wrong fragment a
// reasonable corrector should flag, not a specific provider's exact
// span, so the same case scores any provider fairly.
//
// A Case with an empty MustCorrect is a false-positive trap (see
// eval/corpus/false-positive-traps.yaml): a fully natural sentence
// that should draw ZERO corrections. scoreCase and the aggregate
// FPRate both key off "MustCorrect is empty" to recognize a trap case,
// rather than a separate IsTrap field, so a trap can never accidentally
// drift out of sync with its own emptiness.
type Case struct {
	ID string `yaml:"id"`
	// Selection is the exact text the teacher agent reviews — verbatim
	// what a learner would have selected in the editor.
	Selection string `yaml:"selection"`
	// Context is optional surrounding document text (the same Context
	// field ReviewInput takes) — empty for most cases, populated where
	// the surrounding text is what makes a phrase natural or unnatural
	// (e.g. a register mismatch that only shows up against a stated
	// audience).
	Context string `yaml:"context"`
	// MustCorrect: substrings that must each appear in some
	// correction's Original. Empty means "false-positive trap — expect
	// no corrections at all."
	MustCorrect []string `yaml:"must_correct"`
	// MustNotCorrect: substrings that must NOT appear in any
	// correction's Original — text that's already correct and must not
	// be re-flagged, even in a case that also expects other, real
	// corrections.
	MustNotCorrect []string `yaml:"must_not_correct"`
	// Notes documents why the case exists — the specific grammar point,
	// naturalness judgment, or trap it's pinning down. Not used by
	// scoring; purely for the humans maintaining the corpus.
	Notes string `yaml:"notes"`
}

// CaseResult is one Case's scored outcome (see scoreCase).
type CaseResult struct {
	ID                          string
	TruePos, FalseNeg, FalsePos int
	Pass                        bool
}

// Report is one `jlp eval` run's full output: written to
// eval/reports/<RFC3339>.md and printed as a summary.
type Report struct {
	Provider, Model, PromptVersion string
	Cases                          []CaseResult
	Precision, Recall, FPRate      float64
}

// scoreCase grades result (the teacher agent's response to c.Selection)
// against c's expectations. It is pure — no I/O, no AI calls — so it's
// exhaustively unit-testable on hand-built correction.Result values
// (see eval_test.go).
//
// TruePos: how many of c.MustCorrect were matched by at least one
// correction's Original (substring match, counted once per
// MustCorrect entry no matter how many corrections match it).
// FalseNeg: the MustCorrect entries no correction matched.
// FalsePos: corrections whose Original contains any MustNotCorrect
// substring, PLUS — for trap cases (c.MustCorrect empty) — every
// correction at all counts once (a trap case allows zero corrections
// period, not just corrections outside some blocklist). A single
// correction is counted at most once even if it satisfies both
// conditions.
// Pass: no misses and no unwanted corrections (FalseNeg == 0 &&
// FalsePos == 0) — for a trap case this reduces to "zero corrections",
// since FalseNeg is always 0 when MustCorrect is empty.
func scoreCase(c Case, result correction.Result) CaseResult {
	truePos := 0
	for _, want := range c.MustCorrect {
		for _, corr := range result.Corrections {
			if strings.Contains(corr.Original, want) {
				truePos++
				break
			}
		}
	}
	falseNeg := len(c.MustCorrect) - truePos

	// A trap case (empty MustCorrect) rejects every correction, not
	// just ones matching MustNotCorrect — see the doc comment above.
	isTrap := len(c.MustCorrect) == 0

	falsePos := 0
	for _, corr := range result.Corrections {
		blocked := false
		for _, bad := range c.MustNotCorrect {
			if strings.Contains(corr.Original, bad) {
				blocked = true
				break
			}
		}
		if blocked || isTrap {
			falsePos++
		}
	}

	return CaseResult{
		ID:       c.ID,
		TruePos:  truePos,
		FalseNeg: falseNeg,
		FalsePos: falsePos,
		Pass:     falseNeg == 0 && falsePos == 0,
	}
}

// aggregate turns a run's per-case results into Report's three summary
// numbers. Pure (see scoreCase's doc comment for why that matters
// here too): runEval calls it once after scoring every case, and
// eval_test.go pins its edge cases (all-zero corpus, no traps, a
// mixed run) directly, without any AI involved.
//
// Precision = TP/(TP+FP), Recall = TP/(TP+FN) — both defined as 1.0
// when their denominator is 0 (no positives expected/produced is
// trivially "no misses"), matching the brief's "1.0 when no positives"
// rule for precision and applied symmetrically to recall. FPRate =
// (trap cases that failed) / (trap cases), 0 when there are no traps
// in the corpus at all (never divides by zero).
//
// cases and results must be index-aligned (as runEval always builds
// them: one CaseResult appended per Case, in order) — aggregate reads
// cases[i].MustCorrect only to recognize a trap case (results[i] alone
// can't distinguish "trap case that passed" from "a normal case that
// happened to have TruePos==FalseNeg==0" without also knowing whether
// MustCorrect was empty).
func aggregate(cases []Case, results []CaseResult) (precision, recall, fpRate float64) {
	var truePos, falseNeg, falsePos int
	var trapTotal, trapFailed int
	for i, cr := range results {
		truePos += cr.TruePos
		falseNeg += cr.FalseNeg
		falsePos += cr.FalsePos
		if len(cases[i].MustCorrect) == 0 {
			trapTotal++
			if !cr.Pass {
				trapFailed++
			}
		}
	}

	precision = ratio(truePos, truePos+falsePos)
	recall = ratio(truePos, truePos+falseNeg)
	if trapTotal > 0 {
		fpRate = float64(trapFailed) / float64(trapTotal)
	}
	return precision, recall, fpRate
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 1.0
	}
	return float64(num) / float64(den)
}

// roundTo rounds x to places decimal digits — used only at the
// runEvalCommand/report boundary (see reportPrecision's doc comment),
// never inside aggregate/scoreCase, which stay full-precision and pure
// for eval_test.go's pinned math to check exactly.
func roundTo(x float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(x*scale) / scale
}

// loadCases parses one corpus file's YAML list of Case entries and
// validates it before returning: every id must be non-empty and every
// selection must be non-empty. Mirrors internal/domain/grammar.LoadCatalog's
// load-then-validate shape (same yaml.v3 decode-into-slice style).
func loadCases(r io.Reader) ([]Case, error) {
	var cases []Case
	if err := yaml.NewDecoder(r).Decode(&cases); err != nil {
		return nil, fmt.Errorf("eval: decode corpus: %w", err)
	}
	for i, c := range cases {
		if c.ID == "" {
			return nil, fmt.Errorf("eval: entry %d: empty id", i)
		}
		if c.Selection == "" {
			return nil, fmt.Errorf("eval: case %q: empty selection", c.ID)
		}
	}
	return cases, nil
}

// loadCorpus reads and concatenates every corpus file in paths (in
// order), rejecting a case id that repeats across files — the whole
// corpus shares one ID namespace, both because the report table lists
// every case together and because a collision almost always means a
// case was copy-pasted into the wrong file.
func loadCorpus(paths ...string) ([]Case, error) {
	var all []Case
	seen := make(map[string]bool)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("eval: open %s: %w", p, err)
		}
		cases, err := loadCases(f)
		closeErr := f.Close()
		if err != nil {
			return nil, fmt.Errorf("eval: %s: %w", p, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("eval: close %s: %w", p, closeErr)
		}
		for _, c := range cases {
			if seen[c.ID] {
				return nil, fmt.Errorf("eval: duplicate case id %q (in %s)", c.ID, p)
			}
			seen[c.ID] = true
		}
		all = append(all, cases...)
	}
	return all, nil
}

// evalSession builds the neutral session ReviewWriting reviews every
// case against: teacher mode "teacher" (not socratic — a socratic
// response withholds Replacement behind a Hint, which scoreCase can't
// grade the same way), balanced strictness, both-language explanations.
// RecentErrors/ConceptCandidates/ExpressionsToEncourage are left empty
// deliberately, per the brief: the eval corpus isolates the teacher's
// raw correction behavior from the adaptive context (priorities,
// concept candidates, vocabulary activation) a real session would
// carry, so a score change reflects the provider/prompt, not which
// learner happened to be logged in.
func evalSession() session.Session {
	return session.Session{
		ID:         evalSessionID,
		IdentityID: evalIdentity,
		Purpose:    "",
		Profile: session.Profile{
			TeacherMode:         "teacher",
			Strictness:          "balanced",
			ExplanationLanguage: "both",
		},
	}
}

// runEval reviews every case in cases through t, scores each response
// with scoreCase, and returns the aggregate Report. Provider/Model are
// taken from the last successful response (every case in one run goes
// through the same configured chain, so they're constant across
// cases barring a mid-run failover — see the doc comment on why a
// failure aborts the whole run rather than skipping the case).
//
// A single case's ReviewWriting error aborts the whole run rather than
// being scored as a miss: a schema-validation failure or provider
// outage is an eval-infrastructure problem, not a finding about
// correction quality, and silently no-scoring it would corrupt
// Precision/Recall by shrinking the denominator without recording why.
func runEval(ctx context.Context, t *teacher.Agent, cases []Case) (Report, error) {
	sess := evalSession()
	results := make([]CaseResult, 0, len(cases))
	report := Report{PromptVersion: evalPromptVersion}

	for _, c := range cases {
		result, resp, err := t.ReviewWriting(ctx, teacher.ReviewInput{
			Identity:  evalIdentity,
			Session:   sess,
			Selection: c.Selection,
			Context:   c.Context,
		})
		if err != nil {
			return Report{}, fmt.Errorf("eval: case %q: review writing: %w", c.ID, err)
		}
		report.Provider = resp.Provider
		report.Model = resp.Model
		results = append(results, scoreCase(c, result))
	}

	report.Cases = results
	report.Precision, report.Recall, report.FPRate = aggregate(cases, results)
	return report, nil
}

// reportSummary mirrors the fenced ```yaml block writeReportMarkdown
// puts at the top of every report file — the machine-parseable form
// parseReportSummary reads back to compare against the current run,
// without parsing the human-oriented Markdown table below it.
type reportSummary struct {
	Timestamp     string  `yaml:"timestamp"`
	Provider      string  `yaml:"provider"`
	Model         string  `yaml:"model"`
	PromptVersion string  `yaml:"prompt_version"`
	Cases         int     `yaml:"cases"`
	Precision     float64 `yaml:"precision"`
	Recall        float64 `yaml:"recall"`
	FPRate        float64 `yaml:"fp_rate"`
}

// writeReportMarkdown renders report as the eval/reports/<ts>.md
// contents: a fenced yaml summary block first (see reportSummary),
// then a human-readable summary, then one row per case. The yaml block
// leads the file specifically so parseReportSummary never has to
// disambiguate it from any other fenced block a future report section
// might add.
func writeReportMarkdown(report Report, ts string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# JLP Eval Report — %s\n\n", ts)
	fmt.Fprintf(&b, "```yaml\n")
	fmt.Fprintf(&b, "timestamp: %s\n", ts)
	fmt.Fprintf(&b, "provider: %s\n", report.Provider)
	fmt.Fprintf(&b, "model: %s\n", report.Model)
	fmt.Fprintf(&b, "prompt_version: %s\n", report.PromptVersion)
	fmt.Fprintf(&b, "cases: %d\n", len(report.Cases))
	fmt.Fprintf(&b, "precision: %.4f\n", report.Precision)
	fmt.Fprintf(&b, "recall: %.4f\n", report.Recall)
	fmt.Fprintf(&b, "fp_rate: %.4f\n", report.FPRate)
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "- Provider / model: %s / %s\n", report.Provider, report.Model)
	fmt.Fprintf(&b, "- Prompt version: %s\n", report.PromptVersion)
	fmt.Fprintf(&b, "- Cases: %d\n", len(report.Cases))
	fmt.Fprintf(&b, "- Precision: %.3f\n", report.Precision)
	fmt.Fprintf(&b, "- Recall: %.3f\n", report.Recall)
	fmt.Fprintf(&b, "- False-positive rate (traps): %.3f\n\n", report.FPRate)

	fmt.Fprintf(&b, "## Per-case results\n\n")
	fmt.Fprintf(&b, "| ID | TruePos | FalseNeg | FalsePos | Pass |\n")
	fmt.Fprintf(&b, "| --- | --- | --- | --- | --- |\n")
	for _, cr := range report.Cases {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %t |\n", cr.ID, cr.TruePos, cr.FalseNeg, cr.FalsePos, cr.Pass)
	}
	return b.String()
}

// writeReport writes report's Markdown to
// evalReportsDir/<ts RFC3339>.md, creating evalReportsDir if needed,
// and returns the path written.
func writeReport(report Report, ts string) (string, error) {
	if err := os.MkdirAll(evalReportsDir, 0o755); err != nil {
		return "", fmt.Errorf("eval: create %s: %w", evalReportsDir, err)
	}
	path := filepath.Join(evalReportsDir, ts+".md")
	if err := os.WriteFile(path, []byte(writeReportMarkdown(report, ts)), 0o644); err != nil {
		return "", fmt.Errorf("eval: write %s: %w", path, err)
	}
	return path, nil
}

// extractYAMLBlock returns the contents of the FIRST ```yaml ... ```
// fenced block in md (writeReportMarkdown always puts the machine-
// parseable summary there — see its doc comment), and false if none is
// found (an old or hand-edited report missing the block).
func extractYAMLBlock(md string) (string, bool) {
	const fence = "```yaml"
	start := strings.Index(md, fence)
	if start == -1 {
		return "", false
	}
	rest := md[start+len(fence):]
	end := strings.Index(rest, "```")
	if end == -1 {
		return "", false
	}
	return rest[:end], true
}

// parseReportSummary reads path and decodes its leading yaml summary
// block into a reportSummary — the previous run's scores, for the
// regression comparison in runEvalCommand below.
func parseReportSummary(path string) (reportSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return reportSummary{}, fmt.Errorf("eval: read %s: %w", path, err)
	}
	block, ok := extractYAMLBlock(string(data))
	if !ok {
		return reportSummary{}, fmt.Errorf("eval: %s: no yaml summary block", path)
	}
	var s reportSummary
	if err := yaml.Unmarshal([]byte(block), &s); err != nil {
		return reportSummary{}, fmt.Errorf("eval: %s: decode summary: %w", path, err)
	}
	return s, nil
}

// latestReportBefore returns the most recent *.md report in dir whose
// RFC3339 timestamp sorts strictly before ts (the current run's own
// timestamp) AND whose persisted provider/model match provider/model
// exactly — i.e. the report the current run's regression check should
// be compared against — or "" if dir doesn't exist yet, or holds no
// such report at all, or holds only reports from OTHER provider/model
// pairs. Comparing precision/recall across different providers or
// models would be comparing unlike things (PRD §49's regression gate
// is about catching a real change in ONE configuration's quality, not
// flagging "provider B scores differently than provider A"), so this
// is a like-with-like filter, not just a chronological one.
//
// RFC3339-with-UTC-Z filenames sort lexically in chronological order,
// so candidates are still found via a plain string comparison over a
// sorted directory listing; each candidate (newest first) is then
// parsed to check its provider/model, stopping at the first match. A
// candidate that fails to parse (e.g. a hand-edited or corrupted
// report) is skipped rather than erroring the whole search — the same
// tolerance runEvalCommand already applies to a single previous
// report that fails to parse.
func latestReportBefore(dir, ts, provider, model string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("eval: read %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	tsFile := ts + ".md"
	for i := len(names) - 1; i >= 0; i-- {
		n := names[i]
		if n >= tsFile {
			continue
		}
		path := filepath.Join(dir, n)
		summary, err := parseReportSummary(path)
		if err != nil {
			slog.Warn("eval: could not parse candidate baseline report, skipping", "path", path, "err", err)
			continue
		}
		if summary.Provider == provider && summary.Model == model {
			return path, nil
		}
	}
	return "", nil
}

// noopAIRequestRepo discards observability.NewAIObserver's audit
// records instead of persisting them to Postgres. buildAIGenerator
// needs a storage.AIRequestRepository to construct the router (every
// provider it wraps records latency/cost/success there), but `jlp
// eval` is meant to run standalone in the tools container with no
// database required — see the Makefile's `eval` target and the
// package doc comment above. A live `jlp serve` process already
// records every AI call's real observability trail via Postgres; eval
// runs are a separate, repeatable benchmark, not learner-facing
// traffic, so there's nothing here worth persisting.
type noopAIRequestRepo struct{}

func (noopAIRequestRepo) Insert(context.Context, storage.AIRequestRecord) error { return nil }

func (noopAIRequestRepo) List(context.Context, learner.IdentityID, int) ([]storage.AIRequestRecord, error) {
	return nil, nil
}

// noComparableBaselineLine is the exact line printEvalSummary prints
// when prev is nil — no prior report shares the current run's
// provider+model (see latestReportBefore) — extracted to a constant so
// eval_test.go's TestRunEvalCommandIgnoresMismatchedProviderBaseline
// can assert the EXACT wording is emitted, not just infer it from
// runEvalCommand returning a nil error.
const noComparableBaselineLine = "  no comparable baseline"

// printEvalSummary writes report's headline numbers to stdout, plus
// the delta against prev (nil when there's no earlier report with the
// SAME provider+model to compare against — see latestReportBefore) —
// the human-readable counterpart to the yaml block writeReportMarkdown
// embeds in the report file itself.
func printEvalSummary(report Report, ts string, prev *reportSummary) {
	fmt.Printf("jlp eval — %s\n", ts)
	fmt.Printf("  provider=%s model=%s prompt_version=%s cases=%d\n", report.Provider, report.Model, report.PromptVersion, len(report.Cases))
	fmt.Printf("  precision=%.3f recall=%.3f fp_rate=%.3f\n", report.Precision, report.Recall, report.FPRate)
	if prev == nil {
		fmt.Println(noComparableBaselineLine)
		return
	}
	fmt.Printf("  vs %s: precision=%+.3f recall=%+.3f fp_rate=%+.3f\n",
		prev.Timestamp, report.Precision-prev.Precision, report.Recall-prev.Recall, report.FPRate-prev.FPRate)
}

// runEvalCommand is `jlp eval` (wrapped by `make eval`): builds the
// CONFIGURED provider chain (not a hardcoded fake — cfg.AI drives
// this exactly like `jlp serve` does, via the same buildAIGenerator),
// loads the corpus, scores it, writes a report, and returns a non-nil
// error — which cmd/jlp/main.go's generic case-handling turns into
// os.Exit(1), same as every other subcommand — when either the run
// itself fails or PRD §49's regression gate trips.
func runEvalCommand(ctx context.Context, cfg config.Config) error {
	// resolver is nil: `jlp eval` is a one-shot, offline corpus run with
	// no database connection to load /settings overrides from (see
	// buildAIGenerator's own doc comment on why nil is a legitimate,
	// intentional value here) — it always scores against cfg's own
	// APP_AI_* values, exactly like before Phase 4 Task S.
	aiGen, err := buildAIGenerator(cfg, noopAIRequestRepo{}, nil)
	if err != nil {
		return fmt.Errorf("eval: build ai generator: %w", err)
	}

	cases, err := loadCorpus(evalCorpusPaths...)
	if err != nil {
		return fmt.Errorf("eval: load corpus: %w", err)
	}

	teacherAgent := teacher.New(aiGen)
	report, err := runEval(ctx, teacherAgent, cases)
	if err != nil {
		return err
	}
	// Round to the same precision writeReportMarkdown persists (see
	// reportPrecision's doc comment) BEFORE comparing against a
	// previously-persisted, already-rounded report below — otherwise a
	// completely unchanged corpus/provider prints a spurious
	// near-zero-but-not-quite delta (e.g. "-0.000") purely from
	// aggregate()'s full-precision result never having round-tripped
	// through a report file the way the prior run's number did.
	// aggregate() itself stays unrounded (see its own doc comment) —
	// this only rounds the copy the CLI path prints/persists/compares.
	report.Precision = roundTo(report.Precision, reportPrecision)
	report.Recall = roundTo(report.Recall, reportPrecision)
	report.FPRate = roundTo(report.FPRate, reportPrecision)

	ts := time.Now().UTC().Format(evalReportTimeLayout)
	prevPath, err := latestReportBefore(evalReportsDir, ts, report.Provider, report.Model)
	if err != nil {
		return fmt.Errorf("eval: find previous report: %w", err)
	}
	var prev *reportSummary
	if prevPath != "" {
		s, err := parseReportSummary(prevPath)
		if err != nil {
			slog.Warn("eval: could not parse previous report, skipping regression check", "path", prevPath, "err", err)
		} else {
			prev = &s
		}
	}

	path, err := writeReport(report, ts)
	if err != nil {
		return err
	}

	printEvalSummary(report, ts, prev)
	fmt.Printf("report written to %s\n", path)

	if prev != nil {
		if prev.Precision-report.Precision > evalRegressionThreshold {
			return fmt.Errorf("eval: regression: precision dropped from %.3f to %.3f (> %.2f threshold, vs %s)",
				prev.Precision, report.Precision, evalRegressionThreshold, prevPath)
		}
		if prev.Recall-report.Recall > evalRegressionThreshold {
			return fmt.Errorf("eval: regression: recall dropped from %.3f to %.3f (> %.2f threshold, vs %s)",
				prev.Recall, report.Recall, evalRegressionThreshold, prevPath)
		}
	}
	return nil
}
