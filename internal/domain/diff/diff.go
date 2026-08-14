package diff

import (
	"unicode/utf8"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// Op represents the operation type in a diff segment.
type Op int

const (
	OpEqual Op = iota
	OpInsert
	OpDelete
)

// Segment represents a single segment in a rune-level diff.
type Segment struct {
	Op   Op
	Text string
}

// Runes computes a rune-level diff between two strings using diffmatchpatch
// with semantic cleanup. It returns segments where each Text is valid UTF-8.
// Properties:
//   - Concatenating non-delete segments equals b
//   - Concatenating non-insert segments equals a
//   - Every Segment.Text is valid UTF-8
//   - Runes(x, x) returns a single OpEqual segment
func Runes(a, b string) []Segment {
	dmp := diffmatchpatch.New()

	// DiffMain returns diffs as []diffmatchpatch.Diff
	// We use checklines=false to work at character level (which treats UTF-8 correctly)
	diffs := dmp.DiffMain(a, b, false)

	// Apply semantic cleanup to group semantically related changes
	// CRITICAL: Must capture return value as DiffCleanupSemantic may reallocate the slice
	diffs = dmp.DiffCleanupSemantic(diffs)

	// Convert diffmatchpatch.Diff to our Segment type
	segments := make([]Segment, 0, len(diffs))
	for _, diff := range diffs {
		var op Op
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			op = OpEqual
		case diffmatchpatch.DiffInsert:
			op = OpInsert
		case diffmatchpatch.DiffDelete:
			op = OpDelete
		}

		// Verify UTF-8 validity - this should never fail with valid input
		if !utf8.ValidString(diff.Text) {
			panic("diffmatchpatch produced invalid UTF-8: " + diff.Text)
		}

		segments = append(segments, Segment{Op: op, Text: diff.Text})
	}

	// Handle edge case: if both strings are identical, return single OpEqual segment
	if a == b {
		return []Segment{{Op: OpEqual, Text: a}}
	}

	// Handle edge case: empty results (shouldn't happen normally)
	if len(segments) == 0 && a == "" && b == "" {
		return []Segment{{Op: OpEqual, Text: ""}}
	}

	return segments
}
