package httpx

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// settingsPages are the configuration areas that must be reachable from
// /settings. The API tokens page was originally reachable ONLY through a
// sentence in the middle of the AI model settings, which is not
// somewhere anyone looks for it — this list is what stops the next one
// being hidden the same way.
var settingsPages = []string{"/settings/models", "/settings/agents", "/settings/tokens"}

func TestSettingsIndexLinksEverySettingsPage(t *testing.T) {
	h := NewServer(testOptions()).HandlerForTest()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, page := range settingsPages {
		if !strings.Contains(body, `href="`+page+`"`) {
			t.Errorf("the settings index does not link to %s, so it can only be found by knowing the URL", page)
		}
	}
}

// Every page the index advertises must actually exist. A card linking to
// a 404 is worse than no card.
//
// Uses settingsTestOptions, which wires the settings service the model
// page needs: with bare testOptions that page 500s on a nil service, and
// a 500 would be a fixture artefact rather than the routing mistake this
// is looking for.
func TestEverySettingsPageResolves(t *testing.T) {
	opts, _ := settingsTestOptions(t)
	h := NewServer(opts).HandlerForTest()
	for _, page := range settingsPages {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, page, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (body %.120s)", page, rec.Code, rec.Body.String())
		}
	}
}

// The model page's forms moved with it. A form still posting to the old
// /settings/{provider}/... address would 404 on save — silently, from
// the learner's point of view, because the browser just shows a 404
// page after clicking 保存.
func TestSettingsFormsPostUnderTheirOwnPage(t *testing.T) {
	body, err := os.ReadFile(repoPath("web", "templates", "models.html.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = `action="/settings/`
	var checked int
	for offset := 0; ; {
		i := strings.Index(string(body)[offset:], marker)
		if i < 0 {
			break
		}
		at := offset + i
		offset = at + len(marker)
		checked++
		rest := string(body)[at:]
		end := strings.Index(rest[len(`action="`):], `"`) + len(`action="`)
		action := rest[len(`action="`):end]
		if !strings.HasPrefix(action, "/settings/models/") {
			t.Errorf("a form on the AI model page posts to %q, which is no longer a route — it must post under /settings/models/", action)
		}
	}
	if checked < 4 {
		t.Fatalf("checked %d form actions, want at least 4 — the scan has drifted from the markup", checked)
	}
	t.Logf("checked %d form actions", checked)
}
