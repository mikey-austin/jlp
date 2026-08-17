package httpx

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// iconCollapsingClasses are the classes whose <=480px rule sets
// font-size: 0 on the control and restores it only for a child marked
// aria-hidden="true" (see components.css). A control carrying one of
// them without such a child renders as a blank circle on a phone.
var iconCollapsingClasses = []string{`class="icon-btn"`, `class="theme-toggle"`}

// TestIconButtonsAllCarryAGlyph guards a bug that only showed on a phone:
// the logout button borrowed .theme-toggle's class without its glyph, so
// at <=480px it collapsed to an empty circle — invisible at desktop width
// and invisible to every other test in this suite.
//
// Implementation note, because two earlier versions of this test were
// wrong in the same direction: both used one regex to match a whole
// <button>…</button> AND its class, and both silently matched only a
// SUBSET of the controls (Go's regexp found 10 elements in the layout
// where the identical pattern found 13 in Python). A test that quietly
// checks less than it claims is worse than no test, so this scans for
// the class strings themselves — there is nothing to under-match — and
// counts what it checked.
func TestIconButtonsAllCarryAGlyph(t *testing.T) {
	root := repoPath("web", "templates")
	var checked int

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(raw)
		rel, _ := filepath.Rel(root, path)

		for _, class := range iconCollapsingClasses {
			for offset := 0; ; {
				i := strings.Index(body[offset:], class)
				if i < 0 {
					break
				}
				at := offset + i
				offset = at + len(class)
				checked++

				// The control ends at whichever closing tag comes first.
				rest := body[at:]
				end := len(rest)
				for _, closer := range []string{"</button>", "</a>"} {
					if j := strings.Index(rest, closer); j >= 0 && j < end {
						end = j
					}
				}
				if !strings.Contains(rest[:end], `aria-hidden="true"`) {
					t.Errorf("%s: a control with %s has no aria-hidden glyph, so it collapses to an empty circle at <=480px:\n%s",
						rel, class, strings.TrimSpace(rest[:end]))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}

	// The logout button and the theme toggle both carry a collapsing
	// class today. Fewer means the scan has drifted from the markup and
	// is checking less than it claims — exactly how the earlier versions
	// passed while covering one control instead of two.
	if checked < 2 {
		t.Fatalf("checked %d icon-collapsing controls, want at least 2 — the scan has drifted from the markup", checked)
	}
	t.Logf("checked %d icon-collapsing controls", checked)
}

// repoPath resolves a path relative to the repository root, derived from
// THIS SOURCE FILE rather than the process working directory.
//
// The obvious filepath.Join("..","..","..", …) is wrong here: `go test`
// in this checkout runs with the working directory set to the module
// root, so those relative paths escaped the worktree and read the main
// checkout instead — a different copy of the repository. A guard test
// that silently validates someone else's files passes for the wrong
// reason, which is worse than not existing.
func repoPath(parts ...string) string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed; cannot locate the repository root")
	}
	// <repo>/internal/<group>/<pkg>/<file>_test.go → up three.
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(append([]string{root}, parts...)...)
}
