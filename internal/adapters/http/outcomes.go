package httpx

import "net/http"

// minimumMeasurableWeeks is how many weeks must actually carry data
// before /outcomes draws a trend line for them. One point is not a
// trend, and a single-point polyline rendered across a full-width
// sparkline reads as a flat line at whatever height that point lands —
// a shape the reader will interpret as "steady", which is a claim
// nothing supports. Below this the page shows an explicit
// not-enough-data message in the chart's place instead. It matches
// application/outcomes' own rule for when the headline mentions the
// fading trend at all, so the sentence and the chart can never
// disagree.
const minimumMeasurableWeeks = 2

// outcomesPage handles GET /outcomes: the capstone view (Phase 4 Task
// 9, PRD §52/§72/§73) — did being corrected lead to better later
// production?
//
// The handler itself makes no judgements: application/outcomes.Analyser
// has already classified every concept and written the headline, so
// everything here is presentation. What this layer DOES own is the
// honesty of the empty states, because that is a rendering decision:
//
//   - a trend with fewer than minimumMeasurableWeeks measurable weeks is
//     not drawn at all, rather than drawn as a misleading flat or
//     falling line,
//   - the excluded-concept count is passed through prominently rather
//     than being left implicit in a table's length,
//   - a week with nothing measured reaches the template as Attempts/
//     Runes == 0 and is rendered "—", never 0%.
func (s *Server) outcomesPage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	report, err := s.opts.Outcomes.Report(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load learning outcomes", http.StatusInternalServerError)
		return
	}

	fadingRates := make([]float64, 0, len(report.Fading))
	measurableFadingWeeks := 0
	for _, w := range report.Fading {
		fadingRates = append(fadingRates, w.Per1000)
		if w.Runes > 0 {
			measurableFadingWeeks++
		}
	}
	measurableCalibrationWeeks := 0
	for _, w := range report.Calibration {
		if w.Attempts > 0 {
			measurableCalibrationWeeks++
		}
	}

	judged := len(report.Improving) + len(report.Persistent) + len(report.Retired)

	Render(w, r, "outcomes", map[string]any{
		"Title":    "成果",
		"Identity": ident,
		"Headline": report.Headline,

		"Improving":  report.Improving,
		"Persistent": report.Persistent,
		"Retired":    report.Retired,
		"Excluded":   report.Excluded,
		// JudgedCount/ExcludedCount are handed over pre-counted so the
		// template never has to imply either by the length of a table it
		// happens to be rendering — the excluded count in particular
		// must be stated as a number, whether or not anyone scrolls the
		// table.
		"JudgedCount":   judged,
		"ExcludedCount": len(report.Excluded),

		"Fading": report.Fading,
		// The chart and the table are gated SEPARATELY and deliberately.
		// A chart below two measurable weeks would imply a trend nothing
		// supports, so it is replaced by an explanation; the table is
		// just the weekly numbers themselves, which imply nothing, so it
		// is shown as soon as there is a single week to put in it —
		// hiding real data because it is not yet enough for a chart
		// would be its own small dishonesty.
		"ShowFadingChart": measurableFadingWeeks >= minimumMeasurableWeeks,
		"ShowFadingTable": measurableFadingWeeks > 0,
		"FadingWeeks":     measurableFadingWeeks,
		"FadingSpark": sparklineVM{
			Subject: "修正率",
			Points:  sparklinePointsFor(fadingRates),
		},

		"Calibration":     report.Calibration,
		"ShowCalibration": measurableCalibrationWeeks > 0,
	})
}
