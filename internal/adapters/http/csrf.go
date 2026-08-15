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
			if csrfReject(r.Method, r.URL.Path, r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin"), r.Host) {
				csrfRejectError(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// stateChangingMethods are the HTTP methods CSRF protection applies to
// unconditionally — GET (and HEAD/OPTIONS) requests are ordinarily
// never blocked, per the brief: a same-origin policy only needs to stop
// a cross-site page from making the SERVER perform a write on the
// victim's behalf, and an ordinary READ-ONLY GET can't do that (and
// blocking every GET would break normal cross-site navigation/linking).
// See mutatingGetPaths below for the narrow, explicit exception this
// blanket rule needs.
var stateChangingMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// mutatingGetPaths is the opt-in, explicit list of GET routes that
// DO perform a state change server-side and therefore need the exact
// same cross-site rejection stateChangingMethods gets — everything
// else about GET stays exactly as before (never checked, so ordinary
// navigation/linking is unaffected). Today this is exactly one route:
// /anki/export.tsv (Phase 3 Task 3, PRD §19) — see
// httpx.ankiExportTSV's doc comment for why it's a GET at all (browser
// "Save As" download ergonomics — only a GET a bare `<a href>` can
// drive gets that) despite marking every approved card exported as a
// side effect. Without this entry, a cross-site page could trigger
// that mutation invisibly (Content-Disposition: attachment means the
// victim's tab never visibly navigates) via something as simple as
// `window.open('https://victim-host/anki/export.tsv')` — ordinary
// read-only GETs have no such consequence, which is exactly why GET is
// otherwise exempt; this route is the one place that assumption
// doesn't hold, so it alone opts back in.
//
// Adding a future mutating GET (there should be very few — POST is
// still the right default for anything that changes state) means
// adding its path here, not changing csrfReject's general GET
// tolerance.
var mutatingGetPaths = map[string]bool{
	"/anki/export.tsv": true,
}

// isProtectedRequest reports whether method+path together need CSRF
// checking at all — every state-changing method, unconditionally, plus
// the narrow mutatingGetPaths exception for an otherwise-exempt GET.
func isProtectedRequest(method, path string) bool {
	return stateChangingMethods[method] || (method == http.MethodGet && mutatingGetPaths[path])
}

// csrfReject is the whole predicate, factored out of the middleware
// itself so a table test can drive it directly against plain strings
// instead of building *http.Request values for every case. It decides
// once, in four explicit, ORDER-SENSITIVE tiers:
//
//  0. !isProtectedRequest(method, path) → never reject. This is GET/
//     HEAD/OPTIONS/... EXCEPT the narrow mutatingGetPaths allowlist —
//     see that var's doc comment for why a handful of GET routes need
//     to opt back into the checks below despite GET's usual exemption.
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
func csrfReject(method, path, secFetchSite, origin, host string) bool {
	if !isProtectedRequest(method, path) {
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
// comment for why this is checked first, unconditionally.
//
// Deliberately NOT scoped to this extension's own ID (Task 2's
// chrome-extension/) — any installed extension whose Origin has one of
// these two schemes passes. That is intentional, not an oversight, and
// an extension-ID allowlist here would not actually change the
// resulting security posture. What this carve-out does and does not
// defend against:
//
//   - What it defends against: CSRF's actual threat model — a
//     cross-site, unprivileged WEB PAGE tricking a victim's browser into
//     submitting a state-changing request that rides the victim's
//     session cookie. A page's own script cannot set or spoof the
//     Origin header (Fetch spec: Origin is FORBIDDEN to script), so it
//     cannot forge chrome-extension://... regardless of which ID would
//     be required — narrowing this to an allowlisted ID would not close
//     any gap a page-driven attacker could otherwise exploit, because
//     that attacker was never able to reach this tier in the first
//     place.
//   - What it does NOT defend against, allowlist or not: any browser
//     extension the user has personally installed and granted HOST
//     PERMISSION for this app's origin to (the same
//     optional_host_permissions grant Task 2's own options page
//     requests — see chrome-extension/README.md's CORS/extension-host-
//     permissions section). That precondition — host permission for
//     this origin — is also exactly what's required to inject a content
//     script into a page already open on this origin, and a
//     content-script-issued fetch/XHR runs in the PAGE's origin, not the
//     extension's: it arrives here as an ordinary SAME-ORIGIN request
//     (Origin matching Host, or no Sec-Fetch-Site cross-site signal at
//     all), which tier 2/3 below pass unconditionally on their own —
//     with no extension-scheme Origin involved, and thus nothing an
//     ID allowlist on THIS function could ever intercept. In other
//     words: any extension capable of tripping this tier at all already
//     has a strictly easier path (same-origin content-script injection)
//     that bypasses CSRF checking entirely, allowlist or not — so
//     restricting isExtensionOrigin to a specific ID would add
//     complexity (a config value, a pinned extension ID to keep in
//     sync) without closing any real gap. It would be security theater.
//
// The actual mitigation for "a different, malicious/compromised
// extension abuses its host permission to this origin" is outside this
// server's control: it's the same one that applies to any extension a
// user grants broad host access to — install only trusted extensions,
// review the permissions an extension requests before granting them,
// and remove ones no longer needed. That trust decision is made once,
// at install/grant time, in the browser's own UI — this server has no
// visibility into it and no way to enforce it after the fact.
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
