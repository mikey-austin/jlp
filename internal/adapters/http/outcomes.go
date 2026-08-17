package httpx

import (
	"net/http"

	"github.com/mikeyaustin/jlp/internal/application/outcomes"
)

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

	// Only weeks with a real rate are plotted. A week with nothing
	// (or almost nothing) submitted for review has no rate, and its
	// Per1000 is 0 — which is the BOTTOM of the sparkline's axis, the
	// best possible rate. Plotting those would draw a run of inactivity
	// as "corrections fell to zero", contradicting the "—" the table
	// beside it prints for the very same weeks.
	fadingSamples := make([]sparkSample, 0, len(report.Fading))
	weeksWithAnyWriting := 0
	for i, w := range report.Fading {
		if w.Runes > 0 {
			weeksWithAnyWriting++
		}
		if w.Measurable {
			fadingSamples = append(fadingSamples, sparkSample{Index: i, Value: w.Per1000})
		}
	}
	measurableFadingWeeks := len(fadingSamples)
	measurableCalibrationWeeks := 0
	for _, w := range report.Calibration {
		if w.Attempts > 0 {
			measurableCalibrationWeeks++
		}
	}

	judged := len(report.Improving) + len(report.Persistent) + len(report.Retired)

	s.render(w, r, "outcomes", map[string]any{
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
		// The chart and the table are gated SEPARATELY and deliberately,
		// on DIFFERENT tests. A chart below two weeks with a real rate
		// would imply a trend nothing supports, so it is replaced by an
		// explanation. The table is the weekly character and correction
		// counts themselves — raw facts, which imply nothing — so it
		// appears as soon as the learner submitted anything at all, even
		// a week too small to carry a rate (that week's rate cell shows
		// "—", while its counts still show). Hiding measured facts
		// because they are not yet enough for a chart would be its own
		// small dishonesty.
		"ShowFadingChart": measurableFadingWeeks >= minimumMeasurableWeeks,
		"ShowFadingTable": weeksWithAnyWriting > 0,
		"FadingWeeks":     measurableFadingWeeks,
		"FadingSpark": sparklineVM{
			Subject: "修正率",
			Points:  sparklineSamplePoints(fadingSamples, len(report.Fading)),
		},

		"Calibration":     report.Calibration,
		"ShowCalibration": measurableCalibrationWeeks > 0,

		// RecentRunes drives the page's activity caveat. A page whose
		// whole claim is honesty has to say when its own numbers are
		// describing a period in which the learner barely wrote.
		"RecentRunes":   report.RecentRunes,
		"RecentlyQuiet": report.RecentRunes < outcomes.MinMeasurableRunes,
	})
}
