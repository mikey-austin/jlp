package httpx

import (
	"html/template"
	"log/slog"
	"net/http"
)

func Render(w http.ResponseWriter, r *http.Request, page string, data any) {
	t, err := template.ParseFiles(
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
