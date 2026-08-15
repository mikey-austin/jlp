package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCSRFRejectTable pins csrfReject's whole decision table directly
// against plain strings — no *http.Request construction needed — per
// the brief's "one exported-for-test function so the table test is
// clean" instruction.
func TestCSRFRejectTable(t *testing.T) {
	const host = "app.example"
	const anyPath = "/sessions" // an ordinary, non-mutating-GET path for every case that isn't specifically testing mutatingGetPaths
	tests := []struct {
		name         string
		method       string
		path         string
		secFetchSite string
		origin       string
		wantReject   bool
	}{
		{"cross_site_via_sec_fetch_site_post", http.MethodPost, anyPath, "cross-site", "", true},
		{"cross_site_via_sec_fetch_site_delete", http.MethodDelete, anyPath, "cross-site", "", true},
		{"cross_site_via_origin_mismatch_post", http.MethodPost, anyPath, "", "https://evil.example", true},
		{"same_origin_post_passes", http.MethodPost, anyPath, "", "https://app.example", false},
		{"same_origin_with_scheme_and_matching_host_passes", http.MethodPut, anyPath, "", "http://app.example", false},
		{"sec_fetch_site_same_site_passes", http.MethodPost, anyPath, "same-site", "", false},
		{"sec_fetch_site_none_passes", http.MethodPost, anyPath, "none", "", false},
		{"sec_fetch_site_same_origin_passes", http.MethodPost, anyPath, "same-origin", "", false},
		{"headerless_passes_curl", http.MethodPost, anyPath, "", "", false},
		{"chrome_extension_origin_passes", http.MethodPost, anyPath, "", "chrome-extension://abcdefghijklmnop", false},
		{"moz_extension_origin_passes", http.MethodPost, anyPath, "", "moz-extension://12345678-abcd-abcd-abcd-1234567890ab", false},
		{"null_origin_passes", http.MethodPost, anyPath, "", "null", false},
		{"get_never_blocked_even_with_cross_site_header", http.MethodGet, anyPath, "cross-site", "", false},
		{"get_never_blocked_even_with_mismatched_origin", http.MethodGet, anyPath, "", "https://evil.example", false},
		{"patch_cross_site_blocked", http.MethodPatch, anyPath, "cross-site", "", true},
		{
			"sec_fetch_site_present_wins_over_mismatched_origin",
			http.MethodPost, anyPath, "same-site", "https://evil.example", false,
		},
		// Extension-scheme Origin must win even when Sec-Fetch-Site says
		// "cross-site" — a genuine extension's fetch to this app is
		// cross-site by the Fetch Metadata spec's own definition, so it
		// can legitimately carry that header value. Origin is a
		// browser-set forbidden header a page's own script cannot forge,
		// so trusting it ahead of Sec-Fetch-Site here cannot be abused by
		// page-driven CSRF (see csrfReject's tier 1 doc comment).
		{
			"sec_fetch_site_cross_site_with_chrome_extension_origin_passes",
			http.MethodPost, anyPath, "cross-site", "chrome-extension://abcdefghijklmnop", false,
		},
		{
			"sec_fetch_site_cross_site_with_moz_extension_origin_passes",
			http.MethodPost, anyPath, "cross-site", "moz-extension://12345678-abcd-abcd-abcd-1234567890ab", false,
		},
		// The opaque Origin: null carve-out must NOT override a genuine
		// cross-site signal: a sandboxed iframe's cross-site POST sends
		// BOTH Origin: null AND Sec-Fetch-Site: cross-site, and must
		// still be rejected.
		{
			"sec_fetch_site_cross_site_with_null_origin_rejected",
			http.MethodPost, anyPath, "cross-site", "null", true,
		},
		// ...but Origin: null must still pass when Sec-Fetch-Site isn't
		// signaling cross-site — the ordinary file://-page / Task 2 shim
		// case.
		{
			"sec_fetch_site_none_with_null_origin_passes",
			http.MethodPost, anyPath, "none", "null", false,
		},
		// mutatingGetPaths (Phase 3 Task 3, PRD §19): /anki/export.tsv is
		// a GET that mutates (marks approved cards exported), so it must
		// get the SAME cross-site rejection any state-changing method
		// would — this is the fix for the CSRF gap a plain "GET is never
		// blocked" rule would otherwise leave on this one route.
		{
			"mutating_get_path_cross_site_rejected",
			http.MethodGet, "/anki/export.tsv", "cross-site", "", true,
		},
		{
			"mutating_get_path_origin_mismatch_rejected",
			http.MethodGet, "/anki/export.tsv", "", "https://evil.example", true,
		},
		{
			"mutating_get_path_same_origin_passes",
			http.MethodGet, "/anki/export.tsv", "", "https://app.example", false,
		},
		{
			"mutating_get_path_headerless_passes",
			http.MethodGet, "/anki/export.tsv", "", "", false,
		},
		// Scoping proof: an ORDINARY (non-listed) GET path must stay
		// completely exempt even with the exact same cross-site signal
		// that mutatingGetPaths now rejects on /anki/export.tsv — this
		// fix must not become a blanket "block cross-site GET" rule.
		{
			"non_mutating_get_path_cross_site_still_passes",
			http.MethodGet, anyPath, "cross-site", "", false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := csrfReject(tt.method, tt.path, tt.secFetchSite, tt.origin, host)
			if got != tt.wantReject {
				t.Errorf("csrfReject(%q, %q, %q, %q, %q) = %v, want %v",
					tt.method, tt.path, tt.secFetchSite, tt.origin, host, got, tt.wantReject)
			}
		})
	}
}

// TestCSRFProtectMiddleware exercises the actual middleware (header
// extraction from *http.Request, status code, and the JSON-vs-plain-
// text response body shape) rather than the pure predicate above.
func TestCSRFProtectMiddleware(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := CSRFProtect()(next)

	t.Run("cross_site_post_to_api_path_gets_json_403", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if called {
			t.Fatal("downstream handler must not be called on a rejected request")
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		wantBody := `{"error":"cross-origin request rejected"}` + "\n"
		if rec.Body.String() != wantBody {
			t.Errorf("body = %q, want %q", rec.Body.String(), wantBody)
		}
	})

	t.Run("cross_site_post_to_html_path_gets_plain_403", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/sessions", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if called {
			t.Fatal("downstream handler must not be called on a rejected request")
		}
		if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "json") {
			t.Errorf("Content-Type = %q, want plain text (not JSON) for a non-/api/ path", ct)
		}
		if !strings.Contains(rec.Body.String(), "cross-origin request rejected") {
			t.Errorf("body = %q, want it to contain the rejection message", rec.Body.String())
		}
	})

	t.Run("same_origin_post_reaches_handler", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/sessions", nil)
		req.Host = "app.example"
		req.Header.Set("Origin", "https://app.example")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want %d", rec.Code, http.StatusOK)
		}
		if !called {
			t.Fatal("downstream handler must be called for a same-origin request")
		}
	})

	t.Run("headerless_get_reaches_handler", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || !called {
			t.Fatalf("code = %d called = %v, want 200/true for a plain GET", rec.Code, called)
		}
	})

	// The mutatingGetPaths fix, end to end through the actual middleware
	// (not just the csrfReject predicate table above): a cross-site GET
	// to /anki/export.tsv must be rejected exactly like a cross-site
	// POST would be, while an ordinary cross-site GET elsewhere still
	// isn't touched.
	t.Run("cross_site_get_to_mutating_export_path_rejected", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/anki/export.tsv", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if called {
			t.Fatal("downstream handler must not be called on a rejected mutating-GET request")
		}
	})

	t.Run("headerless_get_to_mutating_export_path_reaches_handler", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/anki/export.tsv", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || !called {
			t.Fatalf("code = %d called = %v, want 200/true for a same-origin/headerless GET to a mutating path", rec.Code, called)
		}
	})
}
