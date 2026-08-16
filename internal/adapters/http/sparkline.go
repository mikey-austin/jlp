package httpx

import (
	"fmt"
	"strings"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// sparklineViewBoxWidth/Height/pad match web/templates/partials/
// sparkline.html.tmpl's `viewBox="0 0 120 24"` exactly — a mismatch
// here would produce a polyline outside (or squashed inside a corner
// of) the SVG's visible area.
const (
	sparklineViewBoxWidth  = 120.0
	sparklineViewBoxHeight = 24.0
	sparklinePad           = 2.0
)

// sparklineVM is what any section rendering a sparkline passes to
// web/templates/partials/sparkline.html.tmpl: a label (used as the
// SVG's accessible name) plus pre-computed polyline points — the
// partial does no arithmetic of its own, per the original task brief's
// "template stays logic-free". Used by the dashboard's 弱点トレンド
// section and by /outcomes' assistance-fading and calibration trends.
type sparklineVM struct {
	Subject string
	Points  string
}

// weaknessTrendVMs converts repository trends into view models, one
// per subject, in the same order WeaknessTrends returned them.
func weaknessTrendVMs(trends []storage.SubjectTrend) []sparklineVM {
	out := make([]sparklineVM, 0, len(trends))
	for _, t := range trends {
		out = append(out, sparklineVM{Subject: t.Subject, Points: sparklinePoints(t.Weeks)})
	}
	return out
}

// sparklinePoints computes an SVG <polyline points="..."> value for a
// series of weekly counts. It is a thin adapter over
// sparklinePointsFor below — the counts are the series values.
func sparklinePoints(weeks []storage.WeeklyCount) string {
	values := make([]float64, len(weeks))
	for i, w := range weeks {
		values[i] = float64(w.Count)
	}
	return sparklinePointsFor(values)
}

// sparklinePointsFor computes an SVG <polyline points="..."> value for
// values, plotted left-to-right (oldest first) across the sparkline's
// full width and scaled to its full height (minus sparklinePad on both
// edges so a peak/trough isn't clipped by the stroke). A series that is
// entirely 0 (max == 0) is drawn as a flat line along the bottom rather
// than dividing by a zero max.
//
// It takes float64 rather than int specifically so /outcomes' fading
// (corrections per 1,000 characters) and calibration (mean confidence,
// correct rate) trends plot through this same helper instead of
// growing a second copy of the arithmetic — the scale is normalised to
// the series' own max, so the unit of the values never matters.
//
// Negative values are not expected from any caller (every series here
// is a count or a non-negative rate) and are not special-cased: they
// would plot below the baseline, which is the honest rendering.
func sparklinePointsFor(values []float64) string {
	if len(values) == 0 {
		return ""
	}

	max := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		max = 1
	}

	n := len(values)
	step := sparklineViewBoxWidth / float64(n-1)
	if n == 1 {
		step = 0
	}
	plotHeight := sparklineViewBoxHeight - 2*sparklinePad

	points := make([]string, n)
	for i, v := range values {
		x := float64(i) * step
		y := sparklinePad + plotHeight*(1-v/max)
		points[i] = fmt.Sprintf("%.1f,%.1f", x, y)
	}
	return strings.Join(points, " ")
}
