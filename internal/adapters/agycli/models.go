package agycli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
)

// ModelLister implements ports/ai.ModelLister by running `agy models`
// (Phase 4 Task M) — a separate, lazily-invoked capability from
// generator's own -p invocations, and a separate type rather than a
// second method on generator so constructing one never implies
// constructing (or invoking) the other.
type ModelLister struct {
	bin string
}

// NewModelLister returns a ModelLister for cfg.Bin (defaulting to
// "agy", same as New). Like generator's own New, this never invokes the
// binary at construction — that only happens inside ListModels, called
// lazily from /settings, never at boot.
func NewModelLister(cfg config.AgyCLI) *ModelLister {
	bin := cfg.Bin
	if bin == "" {
		bin = "agy"
	}
	return &ModelLister{bin: bin}
}

// listModelsWaitDelay bounds how long Wait may block after ctx's
// deadline kills the process group, mirroring generator's own
// WaitDelay discipline (agycli.go) so a stray pipe holder can never
// wedge this call past its caller's timeout.
const listModelsWaitDelay = 5 * time.Second

// ListModels runs `agy models` and parses its stdout. Live output (agy
// 1.1.9, observed on this machine) is a "Fetching available
// models..." header line with no tab, followed by one
// "<model-id>\t<display name>" line per model — this takes the field
// before the first tab on each line and silently skips any line
// without one, which discards the header without having to match its
// exact wording. ctx bounds the whole invocation; the caller
// (application/settings.Service) is expected to attach a short
// timeout, and this method kills the process group when ctx ends
// rather than leaving it to exit on its own (same discipline as
// generator.GenerateStructured).
func (l *ModelLister) ListModels(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, l.bin, "models")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = listModelsWaitDelay

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("agycli: list models: %w", ctx.Err())
	}
	if runErr != nil {
		return nil, fmt.Errorf("agycli: list models: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}

	var names []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		idx := strings.IndexByte(line, '\t')
		if idx < 0 {
			continue
		}
		if name := strings.TrimSpace(line[:idx]); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("agycli: list models: no models parsed from output: %s", strings.TrimSpace(stdout.String()))
	}
	return names, nil
}
