package httpx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNonSoftDeleteTriggersOverrideTheDialogCopy guards a promise the
// shared confirmation dialog makes.
//
// Its default text says the record survives and "あとから元に戻せます" —
// you can undo this. That is true of sessions, words and lessons, which
// are soft-deleted and have restore endpoints. It was NOT true of the
// first other user: revoking an API token is irreversible, and the
// dialog cheerfully told the learner it wasn't. Found by opening the
// dialog and reading it, not by any test.
//
// So: a trigger that does not post to a /delete endpoint must supply its
// own copy. The rule is mechanical, which is the point — the next
// non-delete use of this dialog cannot inherit a false promise by
// default.
func TestNonSoftDeleteTriggersOverrideTheDialogCopy(t *testing.T) {
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

			action := body[offset:]
			if j := strings.IndexByte(action, '"'); j >= 0 {
				action = action[:j]
			}

			start := strings.LastIndex(body[:at], "<")
			end := strings.Index(body[at:], ">")
			if start < 0 || end < 0 {
				t.Errorf("%s: could not bound the trigger tag", rel)
				continue
			}
			tag := body[start : at+end]

			// Soft delete: the default copy is accurate, and a restore
			// endpoint exists to back it up.
			//
			// The action is usually a literal ending in /delete. On
			// /vocabulary it is the template expression
			// {{.DeleteAction}}, whose target this scan cannot see — so
			// an expression is accepted when it NAMES a delete action.
			// That is the honest limit of reading templates as text: the
			// alternative is asserting nothing about that page at all.
			if strings.HasSuffix(action, "/delete") {
				continue
			}
			if strings.Contains(action, "{{") && strings.Contains(action, "Delete") {
				continue
			}
			if !strings.Contains(tag, "data-confirm-delete-note") {
				t.Errorf("%s: the trigger posting to %q reuses the dialog's default copy, which promises the record survives and the action can be undone. That is only true for soft delete. Supply data-confirm-delete-note (and -title/-confirm) saying what this actually does:\n%s",
					rel, action, strings.TrimSpace(tag))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}

	// Three soft-delete triggers plus the token revoke.
	if checked < 4 {
		t.Fatalf("checked %d confirm-delete triggers, want at least 4 — the scan has drifted from the markup", checked)
	}
	t.Logf("checked %d confirm-delete triggers", checked)
}
