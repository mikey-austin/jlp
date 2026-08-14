package diff

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRunes(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
	}{
		{
			name: "japanese_prd_example",
			a:    "とても面白いでした",
			b:    "とても面白かったです",
		},
		{
			name: "same_strings",
			a:    "hello",
			b:    "hello",
		},
		{
			name: "empty_strings",
			a:    "",
			b:    "",
		},
		{
			name: "insert_only",
			a:    "hello",
			b:    "hello world",
		},
		{
			name: "delete_only",
			a:    "hello world",
			b:    "hello",
		},
		{
			name: "complex_japanese",
			a:    "いいですいいです",
			b:    "いいですよかったです",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Runes(tt.a, tt.b)

			if len(got) == 0 {
				t.Errorf("Runes(%q, %q) returned empty segments", tt.a, tt.b)
			}

			// Verify concatenation properties
			t.Run("concat_non_delete_equals_b", func(t *testing.T) {
				var buf strings.Builder
				for _, seg := range got {
					if seg.Op != OpDelete {
						buf.WriteString(seg.Text)
					}
				}
				if result := buf.String(); result != tt.b {
					t.Errorf("Concatenating non-delete segments: got %q, want %q", result, tt.b)
				}
			})

			t.Run("concat_non_insert_equals_a", func(t *testing.T) {
				var buf strings.Builder
				for _, seg := range got {
					if seg.Op != OpInsert {
						buf.WriteString(seg.Text)
					}
				}
				if result := buf.String(); result != tt.a {
					t.Errorf("Concatenating non-insert segments: got %q, want %q", result, tt.a)
				}
			})

			// Verify UTF-8 validity
			t.Run("valid_utf8", func(t *testing.T) {
				for i, seg := range got {
					if !utf8.ValidString(seg.Text) {
						t.Errorf("Segment %d has invalid UTF-8: %q", i, seg.Text)
					}
				}
			})
		})
	}
}

func TestRunesSameString(t *testing.T) {
	// Runes(x, x) should yield a single OpEqual segment
	tests := []string{
		"hello",
		"世界",
		"こんにちは世界",
		"",
	}

	for _, str := range tests {
		t.Run(str, func(t *testing.T) {
			result := Runes(str, str)
			if len(result) != 1 {
				t.Errorf("Runes(%q, %q) returned %d segments, want 1", str, str, len(result))
			}
			if len(result) > 0 {
				if result[0].Op != OpEqual {
					t.Errorf("Runes(%q, %q) segment 0: Op = %v, want OpEqual", str, str, result[0].Op)
				}
				if result[0].Text != str {
					t.Errorf("Runes(%q, %q) segment 0: Text = %q, want %q", str, str, result[0].Text, str)
				}
			}
		})
	}
}
