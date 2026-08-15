package httpx

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
)

// funcs are the helpers every page/partial template can call.
// html/template has no arithmetic (or string-casing) of its own, so
// display conversions live here once rather than being recomputed ad
// hoc in each handler that needs them: "percent" turns the dashboard's
// 納得率 ratio into a display percentage, and "lower" turns a JLPT
// chip's display text ("N5") into the lowercase suffix Task 9's
// per-level badge classes use (.badge--jlpt-n5).
var funcs = template.FuncMap{
	"percent": func(ratio float64) string { return fmt.Sprintf("%.0f", ratio*100) },
	"lower":   strings.ToLower,
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
