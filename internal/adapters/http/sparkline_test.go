package httpx

import (
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// TestSparklinePointsPinnedExample pins sparklinePoints' exact output
// for a known 4-week series (viewBox 120x24, pad 2) — the arithmetic
// the sparkline partial deliberately does none of itself, so this is
// the one place that math is verified.
func TestSparklinePointsPinnedExample(t *testing.T) {
	weeks := []storage.WeeklyCount{{Count: 0}, {Count: 2}, {Count: 1}, {Count: 4}}
	got := sparklinePoints(weeks)
	want := "0.0,22.0 40.0,12.0 80.0,17.0 120.0,2.0"
	if got != want {
		t.Fatalf("sparklinePoints(%+v) = %q, want %q", weeks, got, want)
	}
}

// TestSparklinePointsAllZeroDrawsFlatLineNotNaN covers the guard
// against dividing by a zero max: every week at 0 must still produce
// finite coordinates (a flat line along the bottom), not NaN/Inf from
// a 0/0 scale factor.
func TestSparklinePointsAllZeroDrawsFlatLineNotNaN(t *testing.T) {
	weeks := []storage.WeeklyCount{{Count: 0}, {Count: 0}}
	got := sparklinePoints(weeks)
	want := "0.0,22.0 120.0,22.0"
	if got != want {
		t.Fatalf("sparklinePoints(all zero) = %q, want %q", got, want)
	}
}

// TestSparklinePointsEmptyReturnsEmptyString covers a subject with no
// week data at all (shouldn't happen given WeaknessTrends always
// returns a fixed 8-entry Weeks slice, but the helper must not panic
// on a zero-length slice regardless).
func TestSparklinePointsEmptyReturnsEmptyString(t *testing.T) {
	if got := sparklinePoints(nil); got != "" {
		t.Fatalf("sparklinePoints(nil) = %q, want empty string", got)
	}
}
