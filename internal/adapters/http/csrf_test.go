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
	tests := []struct {
		name         string
		method       string
		secFetchSite string
		origin       string
		wantReject   bool
	}{
		{"cross_site_via_sec_fetch_site_post", http.MethodPost, "cross-site", "", true},
		{"cross_site_via_sec_fetch_site_delete", http.MethodDelete, "cross-site", "", true},
		{"cross_site_via_origin_mismatch_post", http.MethodPost, "", "https://evil.example", true},
		{"same_origin_post_passes", http.MethodPost, "", "https://app.example", false},
		{"same_origin_with_scheme_and_matching_host_passes", http.MethodPut, "", "http://app.example", false},
		{"sec_fetch_site_same_site_passes", http.MethodPost, "same-site", "", false},
		{"sec_fetch_site_none_passes", http.MethodPost, "none", "", false},
		{"sec_fetch_site_same_origin_passes", http.MethodPost, "same-origin", "", false},
		{"headerless_passes_curl", http.MethodPost, "", "", false},
		{"chrome_extension_origin_passes", http.MethodPost, "", "chrome-extension://abcdefghijklmnop", false},
		{"moz_extension_origin_passes", http.MethodPost, "", "moz-extension://12345678-abcd-abcd-abcd-1234567890ab", false},
		{"null_origin_passes", http.MethodPost, "", "null", false},
		{"get_never_blocked_even_with_cross_site_header", http.MethodGet, "cross-site", "", false},
		{"get_never_blocked_even_with_mismatched_origin", http.MethodGet, "", "https://evil.example", false},
		{"patch_cross_site_blocked", http.MethodPatch, "cross-site", "", true},
		{
			"sec_fetch_site_present_wins_over_mismatched_origin",
			http.MethodPost, "same-site", "https://evil.example", false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := csrfReject(tt.method, tt.secFetchSite, tt.origin, host)
			if got != tt.wantReject {
				t.Errorf("csrfReject(%q, %q, %q, %q) = %v, want %v",
					tt.method, tt.secFetchSite, tt.origin, host, got, tt.wantReject)
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
}
