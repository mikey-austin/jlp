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

// weaknessTrendVM is what the dashboard's 弱点トレンド section renders per
// subject: the raw subject label plus pre-computed SVG polyline points
// — web/templates/partials/sparkline.html.tmpl does no arithmetic of
// its own, per the task brief's "template stays logic-free".
type weaknessTrendVM struct {
	Subject string
	Points  string
}

// weaknessTrendVMs converts repository trends into view models, one
// per subject, in the same order WeaknessTrends returned them.
func weaknessTrendVMs(trends []storage.SubjectTrend) []weaknessTrendVM {
	out := make([]weaknessTrendVM, 0, len(trends))
	for _, t := range trends {
		out = append(out, weaknessTrendVM{Subject: t.Subject, Points: sparklinePoints(t.Weeks)})
	}
	return out
}

// sparklinePoints computes an SVG <polyline points="..."> value for
// weeks, plotted left-to-right (oldest first) across the sparkline's
// full width and scaled to its full height (minus sparklinePad on both
// edges so a peak/trough isn't clipped by the stroke). A subject with
// every week at 0 (max == 0) is drawn as a flat line along the bottom
// rather than dividing by a zero max.
func sparklinePoints(weeks []storage.WeeklyCount) string {
	if len(weeks) == 0 {
		return ""
	}

	max := 0
	for _, w := range weeks {
		if w.Count > max {
			max = w.Count
		}
	}
	if max == 0 {
		max = 1
	}

	n := len(weeks)
	step := sparklineViewBoxWidth / float64(n-1)
	if n == 1 {
		step = 0
	}
	plotHeight := sparklineViewBoxHeight - 2*sparklinePad

	points := make([]string, n)
	for i, w := range weeks {
		x := float64(i) * step
		y := sparklinePad + plotHeight*(1-float64(w.Count)/float64(max))
		points[i] = fmt.Sprintf("%.1f,%.1f", x, y)
	}
	return strings.Join(points, " ")
}
