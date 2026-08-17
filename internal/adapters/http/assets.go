package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"
)

// Content-addressed static assets.
//
// Every /static/ URL a page emits carries a hash of that file's bytes:
// /static/css/app.css is served to the browser as
// /static/css/app.<hash>.css. A changed file is therefore a changed URL,
// which is a different cache entry, so a stale copy can never be handed
// to a learner who has the old one — and an unchanged file keeps its URL
// and can be cached for a year.
//
// This replaces a hand-maintained version constant in the service
// worker. That constant had to be bumped by whoever edited an asset, and
// it was missed twice in two days: once leaving the 削除 buttons inert,
// once leaving the logout button unstyled after .icon-btn shipped. Both
// were found by eye, in a browser, after deploying. The version below is
// derived from the assets themselves, so there is nothing left to
// remember.
// 12 hex chars of SHA-256. Long enough that an accidental collision
// between two versions of the same file is not a thing that happens;
// short enough to keep URLs readable in the network tab.
const fingerprintLen = 12

// assetRoot is a var only so tests can point it at a temporary tree and
// exercise "an asset changed" without editing files under version
// control. Production never assigns it.
var assetRoot = "web/static"

type assetEntry struct {
	url     string
	modTime time.Time
	size    int64
}

type assetIndex struct {
	mu    sync.Mutex
	cache map[string]assetEntry
}

// assets is package-level to match funcs in render.go: the fingerprint
// of a file on disk is a property of the file, not of a Server, and the
// template helper needs it without threading state through every
// handler.
var assets = &assetIndex{cache: map[string]assetEntry{}}

// URL maps a logical asset path ("/static/css/app.css") to its
// content-addressed one ("/static/css/app.<hash>.css").
//
// It stats the file on every call and re-hashes only when mtime or size
// moved. That keeps the dev loop honest — templates and CSS are already
// read from disk per request, so an edit shows up on refresh without a
// restart — while costing production one stat per asset per render.
func (a *assetIndex) URL(logical string) string {
	full, ok := assetPath(logical)
	if !ok {
		return logical
	}
	info, err := os.Stat(full)
	if err != nil {
		// A missing asset cannot be fingerprinted. Emit the plain path so
		// the page still renders and the problem surfaces as a visible
		// 404 rather than as a blank page or a panic at render time.
		slog.Warn("asset: not found, serving unfingerprinted", "path", logical, "err", err)
		return logical
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if e, hit := a.cache[logical]; hit && e.modTime.Equal(info.ModTime()) && e.size == info.Size() {
		return e.url
	}
	sum, err := hashFile(full)
	if err != nil {
		slog.Warn("asset: unreadable, serving unfingerprinted", "path", logical, "err", err)
		return logical
	}
	url := insertFingerprint(logical, sum)
	a.cache[logical] = assetEntry{url: url, modTime: info.ModTime(), size: info.Size()}
	return url
}

// assetPath resolves a /static/ URL to a file beneath assetRoot,
// refusing anything that escapes it.
func assetPath(logical string) (string, bool) {
	rel, ok := strings.CutPrefix(logical, "/static/")
	if !ok || rel == "" {
		return "", false
	}
	// path.Clean collapses any ".." before it can be turned into a
	// filesystem path; a result that still escapes is not ours to serve.
	clean := path.Clean("/" + rel)
	return filepath.Join(assetRoot, filepath.FromSlash(clean)), true
}

func hashFile(full string) (string, error) {
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:fingerprintLen], nil
}

// insertFingerprint puts the hash before the extension, so
// htmx.min.js becomes htmx.min.<hash>.js and the extension — which is
// what the file server maps to a Content-Type — stays last.
func insertFingerprint(logical, sum string) string {
	ext := path.Ext(logical)
	return strings.TrimSuffix(logical, ext) + "." + sum + ext
}

// stripFingerprint is insertFingerprint's inverse. It reports whether a
// fingerprint was present so the handler can tell a content-addressed
// request (safe to cache forever) from a plain one (must revalidate).
func stripFingerprint(urlPath string) (clean, sum string, ok bool) {
	ext := path.Ext(urlPath)
	base := strings.TrimSuffix(urlPath, ext)
	i := strings.LastIndex(base, ".")
	if i < 0 {
		return urlPath, "", false
	}
	candidate := base[i+1:]
	if len(candidate) != fingerprintLen || !isHex(candidate) {
		return urlPath, "", false
	}
	return base[:i] + ext, candidate, true
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// assetHandler serves /static/*, translating a fingerprinted request
// back to the file on disk and choosing the cache policy.
func assetHandler(files http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean, _, fingerprinted := stripFingerprint(r.URL.Path)
		if !fingerprinted {
			// Reached either by a URL no template emits (the woff2 files,
			// which fonts.css references directly) or by an old page held
			// in a browser's back/forward cache. Both must revalidate:
			// nothing here is content-addressed, so nothing here may be
			// assumed current. "no-cache" still allows a 304 — it means
			// "ask first", not "don't store".
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
			return
		}

		if assets.URL(clean) == r.URL.Path {
			// The hash matches what this file hashes to right now, so
			// this URL can only ever mean these bytes.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// A fingerprint from an older deploy. Serve the current file
			// rather than 404 — a learner mid-navigation on a cached page
			// should not hit a broken stylesheet — but never let this URL
			// be stored, because it no longer names its own contents.
			w.Header().Set("Cache-Control", "no-store")
		}

		// Rewrite onto the real filename for the file server, without
		// mutating the caller's request.
		r2 := r.Clone(r.Context())
		r2.URL.Path = clean
		files.ServeHTTP(w, r2)
	})
}

// precacheAssets is the PWA shell: what the service worker stores so the
// app opens without a network. Paths are logical; the fingerprints are
// applied when the worker is rendered.
var precacheAssets = []string{
	"/offline",
	"/static/fonts/fonts.css",
	"/static/fonts/instrument-sans-latin-400-normal.woff2",
	"/static/fonts/instrument-sans-latin-500-normal.woff2",
	"/static/fonts/instrument-sans-latin-600-normal.woff2",
	"/static/fonts/instrument-sans-latin-700-normal.woff2",
	"/static/fonts/jetbrains-mono-latin-400-normal.woff2",
	"/static/fonts/jetbrains-mono-latin-500-normal.woff2",
	"/static/fonts/jetbrains-mono-latin-700-normal.woff2",
	"/static/css/tokens.css",
	"/static/css/components.css",
	"/static/css/app.css",
	"/static/js/htmx.min.js",
	"/static/js/alpine.min.js",
	"/static/js/theme.js",
	"/static/js/app.js",
	"/static/js/record.js",
	"/static/manifest.webmanifest",
	"/static/icons/icon.svg",
}

// shell is what the service-worker template renders.
type shell struct {
	CacheName string
	URLs      []string
}

// currentShell resolves the precache list to fingerprinted URLs and
// names the cache after them.
//
// Because every /static/ URL already contains a hash of its file, the
// hash OF THE URL LIST is a hash of every byte the worker will store:
// change any precached file and the cache name changes with it. That is
// the whole reason the manual version constant is gone.
//
// /offline is a rendered route rather than a file, so its template is
// hashed in directly — otherwise editing the offline page would leave
// every existing learner with the old one, which is exactly the class of
// bug this is here to prevent.
func currentShell() shell {
	urls := make([]string, 0, len(precacheAssets))
	h := sha256.New()
	for _, a := range precacheAssets {
		u := a
		if strings.HasPrefix(a, "/static/") {
			u = assets.URL(a)
		}
		urls = append(urls, u)
		io.WriteString(h, u+"\n")
	}
	if sum, err := hashFile(filepath.Join("web", "templates", "offline.html.tmpl")); err == nil {
		io.WriteString(h, "offline:"+sum+"\n")
	}
	return shell{
		CacheName: "jlp-shell-" + hex.EncodeToString(h.Sum(nil))[:fingerprintLen],
		URLs:      urls,
	}
}

// serviceWorker renders web/templates/sw.js.tmpl.
//
// The worker is generated rather than static because a service worker is
// only reinstalled when its own script's bytes change. A static worker
// pointing at a manifest it fetched at runtime would never notice the
// manifest moving: the browser would keep the installed copy. Rendering
// the fingerprinted URLs into the script makes an asset change a script
// change, which is what triggers the update.
func serviceWorker(w http.ResponseWriter, r *http.Request) {
	t, err := texttemplate.ParseFiles(filepath.Join("web", "templates", "sw.js.tmpl"))
	if err != nil {
		slog.Error("service worker: parse", "err", err)
		http.Error(w, "service worker unavailable", http.StatusInternalServerError)
		return
	}
	// Without this header a worker served from /static/ may only control
	// /static/*; the app needs scope '/'.
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	// The worker names every other cache entry, so it is the one thing
	// that must never itself be served from cache.
	w.Header().Set("Cache-Control", "no-cache")
	if err := t.Execute(w, currentShell()); err != nil {
		slog.Error("service worker: execute", "err", err)
	}
}
