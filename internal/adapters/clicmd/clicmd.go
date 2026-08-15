// Package clicmd implements ai.StructuredGenerator by shelling out to a
// locally-installed AI CLI tool — the Claude Code CLI or the OpenAI
// Codex CLI — as a host-mode AI fallback (PRD §23: "CLI adapters are
// legitimate first-class adapters"). Both adapters share one exec
// core (this file): render the prompt onto the tool's stdin, run it
// under a bounded context timeout, and extract the first balanced
// JSON object out of whatever text the tool answers with. claude.go
// and codex.go each supply only what differs — the binary name, its
// CLI flags, and how to peel the tool's own stdout framing back to
// plain text before the shared balanced-JSON extraction runs.
//
// Host-mode only: neither `claude` nor `codex` is installed inside the
// app's container image (see the Dockerfile / docker-compose.yml) —
// these adapters exist for running `go run ./cmd/jlp` directly on a
// host that has the CLI installed, or a future bridge that proxies
// into one, per PRD §23's framing of CLI tools as a fallback path, not
// the primary one. NewClaude and NewCodex therefore never fail at
// construction time: a missing binary only becomes a problem when
// GenerateStructured actually tries to run it, and it fails cleanly
// then (an error wrapping something like `exec: "claude": executable
// file not found in $PATH`), not at boot. That makes it safe to wire
// both into cmd/jlp/ai.go's provider map unconditionally — an operator
// who never routes to them inside a container pays nothing for their
// presence, and one who runs the binary on a host gets a working
// fallback with no extra wiring beyond APP_AI_ROUTES.
package clicmd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// model is the fixed Model value both adapters report. Neither CLI
// tool's non-interactive JSON output names which model actually
// answered (`claude -p --output-format json`'s envelope carries only
// .result; `codex exec --json`'s event stream doesn't name one
// either), and the CLI itself may pick a different model per
// invocation depending on the operator's own local config — "cli" is
// an honest placeholder, not a guess at whatever model actually ran.
const model = "cli"

// execTimeout bounds every CLI invocation. 120s is generous enough for
// a real Claude/Codex CLI turn — which may itself span several tool
// calls before answering — while still failing a hung or misbehaving
// process well before it could hang the caller indefinitely.
const execTimeout = 120 * time.Second

// extractFunc turns a CLI's raw stdout bytes into the plain text the
// shared balanced-JSON step (in GenerateStructured, below) should
// search for a {...} object in. claude.go and codex.go each supply
// one: claude's stdout is a JSON envelope to unwrap first; codex's is
// raw text whose trailing line carries the answer.
type extractFunc func(stdout []byte) ([]byte, error)

// generator is the shared exec core both NewClaude and NewCodex build
// on top of. Every field is set once at construction and never
// mutated afterward.
type generator struct {
	bin      string
	args     []string
	provider string
	extract  extractFunc
}

// GenerateStructured renders req onto the CLI's stdin (System + "\n\n"
// + User + "\n\nRespond with ONLY a JSON object matching this
// schema:\n" + Schema), runs the configured binary under a bounded
// context timeout, and extracts the first balanced JSON object from
// its answer.
//
// It does not itself validate that JSON against req.Schema — same
// division of labor as every other ai.StructuredGenerator
// (adapters/anthropic, adapters/ollama): schema validation and
// bounded repair/retry are aiutil.ValidateWithRepairAndRetry's job,
// one layer up, not an individual adapter's.
func (g *generator) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	start := time.Now()

	// context.WithTimeout takes the earlier of ctx's own deadline (if
	// any) and now+execTimeout — a caller-supplied shorter deadline
	// still governs, execTimeout only ever tightens, never loosens, the
	// bound the caller asked for.
	execCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	prompt := req.System + "\n\n" + req.User +
		"\n\nRespond with ONLY a JSON object matching this schema:\n" + string(req.Schema)

	cmd := exec.CommandContext(execCtx, g.bin, g.args...)
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// The CLI's own child process is a shell/wrapper that may itself
	// spawn further subprocesses (a real risk for a Claude/Codex CLI
	// turn, which can shell out to tools of its own). exec.CommandContext's
	// DEFAULT cancellation only kills the direct child; if a
	// grandchild survives it, that grandchild keeps holding the stdout
	// pipe's write end open, and cmd.Wait() blocks on that open pipe
	// until the grandchild eventually exits on its own — silently
	// defeating the whole point of execTimeout. Setpgid puts the whole
	// process tree in its own group, and Cancel sends SIGKILL to the
	// group (negative pid) instead of just the one process, so
	// cancellation actually bounds real wall-clock time as documented.
	// WaitDelay is a second line of defense: if the group-kill somehow
	// still leaves an orphaned pipe holder, Wait gives up on the I/O
	// copy after this long and returns anyway rather than hanging
	// forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Run(); err != nil {
		if execCtx.Err() != nil {
			// The process was killed because the deadline elapsed —
			// wrap execCtx.Err() (context.DeadlineExceeded, normally)
			// directly so callers can detect it with errors.Is
			// regardless of exactly how os/exec itself phrases the
			// underlying kill/wait error.
			return ai.StructuredResponse{Provider: g.provider, Model: model},
				fmt.Errorf("clicmd: %s: %w", g.provider, execCtx.Err())
		}
		return ai.StructuredResponse{Provider: g.provider, Model: model},
			fmt.Errorf("clicmd: %s: %w (stderr: %s)", g.provider, err, bytes.TrimSpace(stderr.Bytes()))
	}

	text, err := g.extract(stdout.Bytes())
	if err != nil {
		return ai.StructuredResponse{Provider: g.provider, Model: model},
			fmt.Errorf("clicmd: %s: %w", g.provider, err)
	}

	obj, ok := aiutil.ExtractJSONObject(string(text))
	if !ok {
		return ai.StructuredResponse{Provider: g.provider, Model: model},
			fmt.Errorf("clicmd: %s: no JSON object found in output: %s", g.provider, bytes.TrimSpace(text))
	}

	return ai.StructuredResponse{
		JSON:     obj,
		Provider: g.provider,
		Model:    model,
		// InputTokens/OutputTokens stay zero: neither CLI's
		// non-interactive JSON output reports token counts, and
		// there's no per-token billing to approximate anyway (PRD
		// §23 frames these as a free, host-installed fallback).
		Latency: time.Since(start),
	}, nil
}
