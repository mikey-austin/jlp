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
// section and by /outcomes' assistance-fading trend. (/outcomes'
// calibration trend renders as a table only — it has no sparkline.)
type sparklineVM struct {
	Subject string
	Points  string
}

// sparkSample is one plotted point: its position within the FULL series
// (0-based) and its value. Keeping the index separate from the slice
// position is what lets a caller plot a series with gaps — /outcomes'
// fading trend has weeks in which nothing was submitted for review, and
// those weeks have no rate at all. Plotting them as 0 would put them at
// the BOTTOM of the axis, i.e. the best possible rate, so a stretch of
// inactivity would read as "corrections fell to zero" — the exact
// misreading the table beside it avoids by printing "—".
type sparkSample struct {
	Index int
	Value float64
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
// a complete series — every entry has a value, so every entry is
// plotted. A series with gaps must go through sparklineSamplePoints
// below instead.
//
// It takes float64 rather than int specifically so /outcomes' fading
// trend (corrections per 1,000 characters) plots through this same
// helper instead of growing a second copy of the arithmetic — the scale
// is normalised to the series' own max, so the unit never matters.
func sparklinePointsFor(values []float64) string {
	samples := make([]sparkSample, len(values))
	for i, v := range values {
		samples[i] = sparkSample{Index: i, Value: v}
	}
	return sparklineSamplePoints(samples, len(values))
}

// sparklineSamplePoints computes an SVG <polyline points="..."> value
// for samples, plotted left-to-right (oldest first) across the
// sparkline's full width and scaled to its full height (minus
// sparklinePad on both edges so a peak/trough isn't clipped by the
// stroke). total is the length of the full series, which fixes the
// horizontal scale: a sample's x position comes from its own Index, so
// omitting a point leaves a gap at the right place on the time axis
// rather than compressing the remaining points together.
//
// A series whose samples are all 0 (max == 0) is drawn as a flat line
// along the bottom rather than dividing by a zero max.
//
// The polyline still connects across an omitted point, which reads as
// "between these two measurements" — the ordinary meaning of a line
// between two data points. That is deliberately different from plotting
// the gap at a value: the line asserts nothing about the missing week,
// whereas a point at 0 would assert the best possible rate for it.
//
// Negative values are not expected from any caller (every series here
// is a count or a non-negative rate) and are not special-cased: they
// would plot below the baseline, which is the honest rendering.
func sparklineSamplePoints(samples []sparkSample, total int) string {
	if len(samples) == 0 || total <= 0 {
		return ""
	}

	max := 0.0
	for _, s := range samples {
		if s.Value > max {
			max = s.Value
		}
	}
	if max == 0 {
		max = 1
	}

	step := sparklineViewBoxWidth / float64(total-1)
	if total == 1 {
		step = 0
	}
	plotHeight := sparklineViewBoxHeight - 2*sparklinePad

	points := make([]string, len(samples))
	for i, s := range samples {
		x := float64(s.Index) * step
		y := sparklinePad + plotHeight*(1-s.Value/max)
		points[i] = fmt.Sprintf("%.1f,%.1f", x, y)
	}
	return strings.Join(points, " ")
}
