package httpx

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
)

// funcs are the helpers every page/partial template can call.
// html/template has no arithmetic of its own, so ratio-to-percentage
// display conversion (e.g. the dashboard's 納得率 tile) lives here once
// rather than being recomputed ad hoc in each handler that needs it.
var funcs = template.FuncMap{
	"percent": func(ratio float64) string { return fmt.Sprintf("%.0f", ratio*100) },
}

func Render(w http.ResponseWriter, r *http.Request, page string, data any) {
	t, err := template.New("layout.html.tmpl").Funcs(funcs).ParseFiles(
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
