package agycli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
)

// writeStub writes an executable shell script to a fresh t.TempDir()
// and returns its absolute path — the same fake-binary pattern
// internal/adapters/clicmd's tests use, and for the same reason: a
// host running these tests may have a REAL `agy` installed, so cfg.Bin
// must point straight at the stub rather than resolving through PATH.
func writeStub(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
	return path
}

// agyModelsStub is `agy models`' real live shape (task brief / observed
// on this machine): a header line with no tab, then one
// "<id>\t<display name>" line per model.
const agyModelsStub = `#!/bin/sh
printf 'Fetching available models...\n'
printf 'gemini-3.6-flash-high\tGemini 3.6 Flash (High)\n'
printf 'claude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)\n'
printf 'gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n'
`

func TestListModelsParsesTabSeparatedLinesAndSkipsHeader(t *testing.T) {
	bin := writeStub(t, "agy", agyModelsStub)
	lister := NewModelLister(config.AgyCLI{Bin: bin})

	got, err := lister.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	want := []string{"gemini-3.6-flash-high", "claude-sonnet-4-6", "gpt-oss-120b-medium"}
	if len(got) != len(want) {
		t.Fatalf("ListModels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListModels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, name := range got {
		if name == "Fetching available models..." {
			t.Errorf("ListModels included the header line: %v", got)
		}
	}
}

func TestListModelsErrorsOnNonZeroExit(t *testing.T) {
	bin := writeStub(t, "agy", "#!/bin/sh\necho 'boom' >&2\nexit 1\n")
	lister := NewModelLister(config.AgyCLI{Bin: bin})

	if _, err := lister.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels = nil error, want one for a non-zero exit")
	}
}

func TestListModelsErrorsWhenBinaryMissing(t *testing.T) {
	lister := NewModelLister(config.AgyCLI{Bin: "jlp-test-definitely-not-a-real-cli-binary-zzz"})
	if _, err := lister.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels = nil error, want one when the binary can't be found")
	}
}

// TestListModelsRespectsContextTimeout pins the "bounded" contract
// (task brief): a hung `agy models` must be killed, not left to block
// forever, once ctx's deadline passes.
func TestListModelsRespectsContextTimeout(t *testing.T) {
	bin := writeStub(t, "agy", "#!/bin/sh\nsleep 5\n")
	lister := NewModelLister(config.AgyCLI{Bin: bin})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := lister.ListModels(ctx)
	if err == nil {
		t.Fatal("ListModels = nil error, want one when the deadline is exceeded")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("ListModels took %s, want well under the stub's 5s sleep (deadline should have killed it)", elapsed)
	}
}
