package writing_test

import (
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/writing"
)

func TestRuneCountCountsRunesNotBytes(t *testing.T) {
	content := "昨日、映画を見た。"
	d := writing.Document{Content: content}

	if got := d.RuneCount(); got != 9 {
		t.Fatalf("RuneCount() = %d, want 9 (byte length is %d)", got, len(content))
	}
}

func TestRuneCountEmptyContent(t *testing.T) {
	d := writing.Document{}
	if got := d.RuneCount(); got != 0 {
		t.Fatalf("RuneCount() = %d, want 0", got)
	}
}
