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
// once, in three explicit, ORDER-SENSITIVE tiers:
//
//  0. Non-state-changing method (GET/HEAD/OPTIONS/...) → never reject.
//
//  1. Extension-scheme Origin (chrome-extension:/moz-extension:) →
//     PASS, unconditionally, regardless of Sec-Fetch-Site. This MUST
//     run first, ahead of tier 2: a genuine browser extension's fetch
//     to this app's http(s) origin is cross-site by the Fetch Metadata
//     spec's own definition (different scheme/origin than the target),
//     so it can legitimately arrive with Sec-Fetch-Site: cross-site —
//     if tier 2 ran first, that header alone would reject Task 2's
//     extension traffic before this carve-out was ever reached. Safe
//     to trust unconditionally because Origin is a browser-set
//     FORBIDDEN header: the Fetch spec forbids a page's own script
//     from setting or spoofing it, so a request presenting an
//     extension-scheme Origin can only have come from a genuine
//     extension (or a non-browser client, which the headerless case
//     below already permits regardless) — a malicious web page mounting
//     a CSRF attack cannot forge this value, so this tier cannot be
//     abused by page-driven CSRF. See isExtensionOrigin.
//
//  2. Sec-Fetch-Site present → trust it exclusively for everything
//     else: reject only when its value is exactly "cross-site"; any
//     other value (same-origin, same-site, none) passes, and the
//     Origin header (if any) is not separately consulted — it's also
//     browser-set/unforgeable, and more precise than Origin when a
//     modern browser sends it.
//
//  3. Sec-Fetch-Site absent → fall back to Origin: present and its
//     host:port matches (or it's the opaque "null") → pass; present
//     and mismatched → reject; absent too (curl, and other non-browser
//     clients that set neither header) → pass. See originMatchesHost
//     for the "null" carve-out's own tradeoff.
//
// Deliberate consequence of this ordering: Origin: null only passes
// via tier 3, which tier 2 can shadow — a sandboxed iframe's cross-site
// POST sends BOTH Origin: null AND Sec-Fetch-Site: cross-site, and
// tier 2 rejects that before tier 3 (or the null carve-out) is ever
// reached. The null carve-out never overrides a genuine cross-site
// signal; it only fires when nothing already said "cross-site".
func csrfReject(method, secFetchSite, origin, host string) bool {
	if !stateChangingMethods[method] {
		return false
	}
	if isExtensionOrigin(origin) {
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

// isExtensionOrigin reports whether origin's scheme is
// chrome-extension: or moz-extension: — see csrfReject's tier 1 doc
// comment for why this is checked first, unconditionally, and why
// that's safe (Origin is an unforgeable, browser-set header).
func isExtensionOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "chrome-extension", "moz-extension":
		return true
	}
	return false
}

// originMatchesHost reports whether origin (the Origin request
// header's value) should be treated as same-origin with host (the
// request's own Host) — csrfReject's tier 3 fallback, reached only
// when Sec-Fetch-Site was absent. Extension-scheme origins are handled
// earlier and unconditionally by isExtensionOrigin (tier 1), so this
// function never sees them.
//
//   - "null": an opaque origin — sent by file:// pages and sandboxed
//     iframes, which have no origin to compare. Accepted here: JLP is
//     a LAN-scoped app (no public internet exposure), and Task 2's
//     file:// verification protocol for the browser extension depends
//     on this passing. The tradeoff: a malicious LOCAL file could also
//     send Origin: null and reach this app if the LAN itself isn't
//     trusted — acceptable for this deployment's threat model, not a
//     general-purpose default. (A cross-site sandboxed-iframe POST,
//     which also sends Origin: null, never reaches this carve-out at
//     all — see csrfReject's "deliberate consequence" note.)
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
