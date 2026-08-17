package httpx

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// funcs are the helpers every page/partial template can call.
// html/template has no arithmetic (or string-casing) of its own, so
// display conversions live here once rather than being recomputed ad
// hoc in each handler that needs them: "percent" turns the dashboard's
// 納得率 ratio into a display percentage, "lower" turns a JLPT chip's
// display text ("N5") into the lowercase suffix Task 9's per-level
// badge classes use (.badge--jlpt-n5), and "days" turns a
// storage.RetrievalItem.Interval time.Duration into the whole-day count
// the /learner 復習キュー table displays (Phase 4 Task 7, PRD §54) —
// every interval the scheduler ever stores is an exact multiple of 24h
// (see application/retrieval's steps table), so integer division never
// loses information here.
// "asset" turns a logical static path into its content-addressed URL
// (/static/css/app.css -> /static/css/app.<hash>.css). Templates must go
// through it rather than writing /static/ paths directly: a plain path
// is cacheable-but-not-versioned, which is how a learner ends up running
// new markup against an old stylesheet. TestTemplatesRouteStaticThroughAsset
// keeps that from creeping back.
var funcs = template.FuncMap{
	"percent": func(ratio float64) string { return fmt.Sprintf("%.0f", ratio*100) },
	"lower":   strings.ToLower,
	"days":    func(d time.Duration) int64 { return int64(d / (24 * time.Hour)) },
	"asset":   assets.URL,
}

// render is what every page handler calls: Render below, plus the
// handful of values the LAYOUT needs and no individual page knows or
// cares about. Today that is the sign-out control's target (empty in
// static and authelia mode, where this app has no session of its own
// to end — see Options.LogoutPath), and the point of funnelling it
// through here is that adding the next such value doesn't mean editing
// twenty handlers' data maps again.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if s.opts.LogoutPath != "" {
		data["LogoutPath"] = s.opts.LogoutPath
	}
	Render(w, r, page, data)
}

// Render executes page against the shared layout. It also loads every
// partial (web/templates/partials/*.html.tmpl) into the same template
// set, so a full page — like anki.html.tmpl's draft/approved lists —
// can embed a partial directly via {{template "anki_card" .}} rather
// than duplicating its markup: the partial then stays the single
// source of truth for that fragment's rendering, whether it's reached
// via a full-page GET or an htmx swap through RenderPartial. No
// partial's defined name collides with "layout" or "content" (checked
// across every existing partial), so folding them into every page's
// template set is safe.
func Render(w http.ResponseWriter, r *http.Request, page string, data any) {
	t, err := template.New("layout.html.tmpl").Funcs(funcs).ParseGlob("web/templates/partials/*.html.tmpl")
	if err != nil {
		slog.Error("template parse", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	t, err = t.ParseFiles(
		"web/templates/layout.html.tmpl",
		"web/templates/"+page+".html.tmpl",
	)
	if err != nil {
		slog.Error("template parse", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("template exec", "page", page, "err", err)
	}
}

// RenderPartial executes one named template from web/templates/partials/*
// directly, without the page layout — for htmx fragment responses like the
// session activity feed.
func RenderPartial(w http.ResponseWriter, r *http.Request, name string, data any) {
	t, err := template.New("partials").Funcs(funcs).ParseGlob("web/templates/partials/*.html.tmpl")
	if err != nil {
		slog.Error("partial parse", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("partial exec", "name", name, "err", err)
	}
}
