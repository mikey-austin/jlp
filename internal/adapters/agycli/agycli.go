// Package agycli implements ai.StructuredGenerator by shelling out to
// the locally-installed `agy` binary (the Antigravity CLI) in print
// mode — a third host-mode CLI adapter alongside adapters/clicmd's
// Claude Code and Codex generators (PRD §23).
//
// It is a separate package rather than a third file in clicmd because
// the invocation discipline genuinely differs, and every difference is
// load-bearing — each one was established by live experiment, not
// guessed:
//
//   - Print mode IGNORES STDIN. The whole prompt document rides argv as
//     the value of -p. clicmd's two CLIs both read stdin; agy silently
//     produces nothing if you pipe to it.
//   - The process runs in a FRESH temp directory, not the server's cwd.
//     Given a real project directory, agy behaves like the coding agent
//     it is and reaches for shell tools to explore it.
//   - `--mode plan` keeps the agent read-only.
//   - GEMINI_API_KEY/GOOGLE_API_KEY are filtered out of the child's
//     environment: the CLI prefers an API key when it sees one, and this
//     adapter exists to use the operator's existing Google login.
//   - Structured output is enforced by the CLI itself via --json-schema,
//     so the answer arrives already parsed in the envelope's
//     structured_output field — no balanced-JSON extraction needed, the
//     way clicmd has to do for its two.
//
// Without the first three, every headless run ends with the CLI
// attempting a tool that needs the "command" permission, which headless
// mode cannot prompt for, and emitting ZERO bytes on stdout — measured
// at 0/6 identical runs before the invocation was corrected, 3/3 after.
// That failure mode is why runAgy treats an empty envelope as an error
// rather than an empty answer.
//
// Host-mode only, same as clicmd: `agy` is not installed in the app's
// container image, so New never fails just because the binary is
// missing — that surfaces per-call instead.
package agycli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const provider = "agycli"

// defaultModelName is what a run reports when no model was pinned: the
// CLI picks its own configured default and the envelope does not name
// it, so this is an honest placeholder rather than a guess.
const defaultModelName = "default"

// defaultTimeout bounds one agy invocation when config supplies none.
// Generous because a plan-mode turn can take a few seconds, but never
// unbounded.
const defaultTimeout = 3 * time.Minute

// maxDocumentBytes bounds the -p argument. argv's OS ceiling is around
// 2 MiB; 1 MiB leaves room for the rest of the command line and fails
// with a clear message instead of a fork/exec E2BIG.
const maxDocumentBytes = 1 << 20

// maxExcerpt caps how much subprocess output an error quotes.
const maxExcerpt = 2000

type generator struct {
	bin      string
	timeout  time.Duration
	resolver ai.ModelResolver

	baseModel, baseEffort string
}

// New returns an ai.StructuredGenerator backed by the agy CLI. Like
// clicmd's constructors it never fails on a missing binary — that is a
// per-call error, so wiring this provider into a container costs
// nothing until something actually routes to it.
func New(cfg config.AgyCLI, resolver ai.ModelResolver) ai.StructuredGenerator {
	bin := cfg.Bin
	if bin == "" {
		bin = "agy"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &generator{
		bin:        bin,
		timeout:    timeout,
		resolver:   resolver,
		baseModel:  cfg.Model,
		baseEffort: cfg.Effort,
	}
}

// envelope is `agy --output-format json`'s stdout. Only the fields this
// adapter reads; confirmed against a live transcript (agy 1.1.9) kept
// in testdata/agy_live.json.
type envelope struct {
	Status string `json:"status"`
	// Response is the assistant's prose. With --json-schema the answer
	// also appears here, appended after any commentary — StructuredOutput
	// is the clean copy and is preferred.
	Response         string          `json:"response"`
	Error            string          `json:"error"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		// The CLI reports cache reads separately from input_tokens; both
		// were really consumed, so both count toward the request's input.
		CacheReadTokens int `json:"cache_read_tokens"`
	} `json:"usage"`
}

// resolveModelEffort returns the model/effort for THIS call: the
// resolver's current override when it has one, else the configured
// values. Called per invocation so a /settings change lands on the next
// request without reconstructing the adapter.
func (g *generator) resolveModelEffort() (model, effort string) {
	model, effort = g.baseModel, g.baseEffort
	if g.resolver == nil {
		return model, effort
	}
	if m, e := g.resolver.Model(provider); m != "" || e != "" {
		if m != "" {
			model = m
		}
		if e != "" {
			effort = e
		}
	}
	return model, effort
}

// clampEffort maps JLP's effort vocabulary onto agy's ladder, which
// stops at "high". A caller asking for xhigh/max means "as hard as this
// backend goes", and passing the literal value would make the CLI
// reject the whole invocation.
func clampEffort(effort string) string {
	switch effort {
	case "xhigh", "max":
		return "high"
	default:
		return effort
	}
}

func (g *generator) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()
	model, effort := g.resolveModelEffort()

	reported := model
	if reported == "" {
		reported = defaultModelName
	}
	fail := func(format string, args ...any) (ai.StructuredResponse, error) {
		return ai.StructuredResponse{Provider: provider, Model: reported},
			fmt.Errorf("agycli: "+format, args...)
	}

	doc := "## Instructions\n" + req.System + "\n\n## Request\n" + req.User + "\n"
	if len(doc) > maxDocumentBytes {
		return fail("prompt document exceeds %d bytes (%d)", maxDocumentBytes, len(doc))
	}

	execCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	// A scratch directory serves two purposes: it is the workspace the
	// CLI sees (so it has no project to go exploring with shell tools),
	// and it holds the schema file --json-schema points at.
	scratch, err := os.MkdirTemp("", "jlp-agycli-*")
	if err != nil {
		return fail("scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	args := []string{
		"-p", doc,
		"--output-format", "json",
		"--mode", "plan",
		"--print-timeout", g.timeout.String(),
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if e := clampEffort(effort); e != "" {
		args = append(args, "--effort", e)
	}
	if len(req.Schema) > 0 {
		schemaPath := filepath.Join(scratch, "schema.json")
		if err := os.WriteFile(schemaPath, req.Schema, 0o600); err != nil {
			return fail("write schema: %w", err)
		}
		args = append(args, "--json-schema", schemaPath)
	}

	cmd := exec.CommandContext(execCtx, g.bin, args...)
	cmd.Dir = scratch
	cmd.Env = filterEnv(os.Environ(), "GEMINI_API_KEY", "GOOGLE_API_KEY")

	// Same process-group discipline as clicmd: the CLI can spawn
	// children that would otherwise keep the stdout pipe open past the
	// deadline, so cancellation kills the whole group and WaitDelay
	// stops Wait from blocking on a stray pipe holder.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	if execCtx.Err() != nil {
		return fail("no reply within %s: %w", g.timeout, execCtx.Err())
	}

	// The permission failure this adapter's invocation shape exists to
	// avoid exits 0 with an empty stdout and an explanation on stderr.
	// Treat it as the hard error it is, so a route falls through to the
	// next provider instead of a caller seeing an empty answer.
	if stdout.Len() == 0 {
		return fail("the CLI produced no output: %s", tailExcerpt(stderr.String()))
	}

	var env envelope
	parseErr := json.Unmarshal(stdout.Bytes(), &env)
	if runErr != nil {
		return fail("%w: %s", runErr, diagnostic(env, parseErr, stdout, stderr))
	}
	if parseErr != nil {
		return fail("unparseable envelope: %v: %s", parseErr, tailExcerpt(stdout.String()))
	}
	if env.Status != "SUCCESS" {
		return fail("the CLI reported %s: %s", env.Status, diagnostic(env, parseErr, stdout, stderr))
	}

	// structured_output is the schema-constrained answer, already parsed
	// by the CLI. Response carries the same JSON but with the agent's
	// commentary in front of it, so prefer the clean copy and fall back
	// only when --json-schema wasn't used or the field came back null.
	answer := env.StructuredOutput
	if len(answer) == 0 || string(answer) == "null" {
		if strings.TrimSpace(env.Response) == "" {
			return fail("the envelope contained no response")
		}
		answer = json.RawMessage(env.Response)
	}

	return ai.StructuredResponse{
		JSON:         answer,
		Provider:     provider,
		Model:        reported,
		InputTokens:  env.Usage.InputTokens + env.Usage.CacheReadTokens,
		OutputTokens: env.Usage.OutputTokens,
		Latency:      time.Since(start),
	}, nil
}

// diagnostic picks the most informative excerpt available for an error.
func diagnostic(env envelope, parseErr error, stdout, stderr bytes.Buffer) string {
	switch {
	case parseErr == nil && strings.TrimSpace(env.Error) != "":
		return tailExcerpt(env.Error)
	case strings.TrimSpace(stderr.String()) != "":
		return tailExcerpt(stderr.String())
	default:
		return tailExcerpt(stdout.String())
	}
}

// filterEnv returns env without any of the named variables.
func filterEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// tailExcerpt keeps the END of subprocess output: agy writes progress
// noise first and the actual failure last, so a head excerpt would show
// only the noise.
func tailExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	if len(s) > maxExcerpt {
		return "..." + s[len(s)-maxExcerpt:]
	}
	return s
}
