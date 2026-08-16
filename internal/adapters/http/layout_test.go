package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLayoutRendersNavHamburgerToggle pins layout.html.tmpl's shared
// topbar (Task NAV): every page must render a real <button> trigger for
// the mobile hamburger menu, with aria-expanded reflecting its (closed)
// initial state and aria-controls pointing at #topnav — the same <nav>
// the desktop layout already renders inline, so the active-link script
// and the nine destinations stay a single source of truth. Exercised
// via GET /sessions since testOptionsWithSessions already wires every
// dependency the layout's content template needs; the topbar itself is
// identical across every page.
func TestLayoutRendersNavHamburgerToggle(t *testing.T) {
	srv := NewServer(testOptionsWithSessions())
	h := srv.HandlerForTest()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `<button type="button" class="nav-toggle" id="nav-toggle" aria-expanded="false" aria-controls="topnav"`) {
		t.Fatalf("missing hamburger trigger with correct ARIA wiring: %s", body)
	}
	if !strings.Contains(body, `id="topnav"`) {
		t.Fatalf("missing #topnav for aria-controls to target: %s", body)
	}
	// The nine destinations stay in the DOM (no hamburger-only duplicate
	// list) — the toggle just changes how #topnav is displayed on small
	// viewports (components.css), not what it contains.
	for _, href := range []string{"/sessions", "/grammar", "/vocabulary", "/practice", "/anki", "/lessons", "/learner", "/outcomes", "/ai"} {
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Fatalf("missing nav destination %q: %s", href, body)
		}
	}
}
