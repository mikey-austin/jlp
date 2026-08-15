package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// CSRFProtect returns middleware that rejects state-changing
// (POST/PUT/PATCH/DELETE) cross-origin requests using origin
// verification only — no CSRF tokens, no session state. Mount it on
// the authenticated route group (server.go), BEFORE route handlers:
// every unauthenticated route (/healthz, /static/*, /offline) is
// GET-only, so there's nothing state-changing to protect there.
//
// See csrfReject for the exact allow/deny rules this enforces.
func CSRFProtect() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if csrfReject(r.Method, r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin"), r.Host) {
				csrfRejectError(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// stateChangingMethods are the HTTP methods CSRF protection applies
// to — GET (and HEAD/OPTIONS) requests are never blocked, per the
// brief: a same-origin policy only needs to stop a cross-site page
// from making the SERVER perform a write on the victim's behalf; a
// cross-site GET can't do that (and blocking it would break normal
// cross-site navigation/linking).
var stateChangingMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// csrfReject is the whole predicate, factored out of the middleware
// itself so a table test can drive it directly against plain strings
// instead of building *http.Request values for every case. It decides
// once, in order:
//
//  1. Non-state-changing method (GET/HEAD/OPTIONS/...) → never reject.
//  2. Sec-Fetch-Site present → trust it exclusively: reject only when
//     its value is exactly "cross-site"; any other value (same-origin,
//     same-site, none) passes, and the Origin header (if any) is not
//     consulted at all — Sec-Fetch-Site is the more precise, harder-to-
//     spoof signal (browser-set, never script-settable) when a modern
//     browser sends it.
//  3. Sec-Fetch-Site absent, Origin present → fall back to comparing
//     Origin's host:port against the request's own Host (see
//     originMatchesHost for the chrome-extension:/moz-extension:/null
//     carve-outs).
//  4. Neither header present (curl, and other non-browser HTTP
//     clients that don't set either) → pass. Browsers reliably send at
//     least one of the two on every fetch/form-submit; a request with
//     neither is, by construction, not a browser page making a
//     cross-site request in the first place.
func csrfReject(method, secFetchSite, origin, host string) bool {
	if !stateChangingMethods[method] {
		return false
	}
	if secFetchSite != "" {
		return secFetchSite == "cross-site"
	}
	if origin == "" {
		return false
	}
	return !originMatchesHost(origin, host)
}

// originMatchesHost reports whether origin (the Origin request
// header's value) should be treated as same-origin with host (the
// request's own Host).
//
//   - "null": an opaque origin — sent by file:// pages and sandboxed
//     iframes, which have no origin to compare. Accepted here: JLP is
//     a LAN-scoped app (no public internet exposure), and Task 2's
//     file:// verification protocol for the browser extension depends
//     on this passing. The tradeoff: a malicious LOCAL file could also
//     send Origin: null and reach this app if the LAN itself isn't
//     trusted — acceptable for this deployment's threat model, not a
//     general-purpose default.
//   - chrome-extension:/moz-extension: schemes: browser extensions
//     send their own extension-ID origin, never the app's — Task 2's
//     browser extension depends on these passing.
//   - anything else: parse origin and compare its Host to host
//     directly (both already in host[:port] form).
func originMatchesHost(origin, host string) bool {
	if origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "chrome-extension", "moz-extension":
		return true
	}
	return u.Host == host
}

// csrfRejectError writes the 403 CSRF-rejection response, mirroring
// requireIdentityError's JSON-for-/api/-plain-text-otherwise shape
// (see middleware.go) so both failure modes look the same to a
// caller.
func csrfRejectError(w http.ResponseWriter, r *http.Request) {
	const msg = "cross-origin request rejected"
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
			slog.Error("write csrf error response", "err", err)
		}
		return
	}
	http.Error(w, msg, http.StatusForbidden)
}
