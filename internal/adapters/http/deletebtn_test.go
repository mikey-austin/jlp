package httpx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteTriggersLookDangerous pins a rule that nothing enforced: a
// control that opens the delete confirmation must be styled as
// destructive. All three of them (vocabulary, sessions, lessons) had
// drifted to .btn--secondary, so the only thing on screen that looked
// dangerous was the confirm button inside the modal — one step too late
// to inform the click that opens it.
//
// The trigger is identified by data-confirm-delete= (the attribute the
// modal script binds to), not by its label, so a new delete surface is
// covered the moment it is wired up rather than when someone remembers
// to add it here.
func TestDeleteTriggersLookDangerous(t *testing.T) {
	const marker = `data-confirm-delete="`
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

		for offset := 0; ; {
			i := strings.Index(body[offset:], marker)
			if i < 0 {
				break
			}
			at := offset + i
			offset = at + len(marker)
			checked++

			// Bound the tag this attribute sits in: back to its '<',
			// forward to its '>'.
			start := strings.LastIndex(body[:at], "<")
			end := strings.Index(body[at:], ">")
			if start < 0 || end < 0 {
				t.Errorf("%s: could not bound the tag around %s", rel, marker)
				continue
			}
			tag := body[start : at+end]

			if !strings.Contains(tag, "btn--danger") {
				t.Errorf("%s: a delete trigger is not styled as destructive (no btn--danger):\n%s",
					rel, strings.TrimSpace(tag))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}

	// Vocabulary, sessions and lessons each have one. A lower count means
	// the scan stopped seeing the markup rather than that the markup is
	// clean — the failure mode that let the icon guard pass while
	// covering half of what it claimed.
	if checked < 3 {
		t.Fatalf("checked %d delete triggers, want at least 3 — the scan has drifted from the markup", checked)
	}
	t.Logf("checked %d delete triggers", checked)
}
