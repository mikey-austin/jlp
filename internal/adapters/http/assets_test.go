package httpx

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempAssetRoot points the asset index at a throwaway tree holding
// every precached path, so "an asset changed" can be exercised without
// editing files under version control.
func withTempAssetRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, logical := range precacheAssets {
		rel, ok := strings.CutPrefix(logical, "/static/")
		if !ok {
			continue // "/offline" is a route, not a file
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("original "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot, oldCache := assetRoot, assets
	assetRoot = dir
	assets = &assetIndex{cache: map[string]assetEntry{}}
	t.Cleanup(func() { assetRoot, assets = oldRoot, oldCache })
	return dir
}

// TestChangingAnAssetChangesItsURLAndTheCacheName is the property the
// whole scheme rests on. The hand-bumped cache constant it replaces was
// missed twice in two days; this asserts the bump can no longer be
// missed, because nobody performs it.
func TestChangingAnAssetChangesItsURLAndTheCacheName(t *testing.T) {
	dir := withTempAssetRoot(t)

	beforeURL := assets.URL("/static/css/components.css")
	beforeShell := currentShell()
	if !strings.Contains(beforeURL, "/static/css/components.") || beforeURL == "/static/css/components.css" {
		t.Fatalf("URL %q is not fingerprinted", beforeURL)
	}

	// Edit one precached file, exactly as the .icon-btn commit did.
	p := filepath.Join(dir, "css", "components.css")
	if err := os.WriteFile(p, []byte(".icon-btn { display: inline-flex }"), 0o644); err != nil {
		t.Fatal(err)
	}

	afterURL := assets.URL("/static/css/components.css")
	afterShell := currentShell()

	if afterURL == beforeURL {
		t.Errorf("editing components.css left its URL at %q — a learner holding the old copy would never refetch it", beforeURL)
	}
	if afterShell.CacheName == beforeShell.CacheName {
		t.Errorf("editing components.css left the service worker cache named %q, so the worker would not reinstall", beforeShell.CacheName)
	}
	if !strings.Contains(strings.Join(afterShell.URLs, " "), afterURL) {
		t.Errorf("the worker precaches %v, which does not include the current %s", afterShell.URLs, afterURL)
	}
}

// An unchanged asset must keep its URL, or every deploy would bust every
// cache and the scheme would buy nothing.
func TestUnchangedAssetsKeepTheirURL(t *testing.T) {
	withTempAssetRoot(t)
	first := currentShell()
	second := currentShell()
	if first.CacheName != second.CacheName {
		t.Errorf("cache name is unstable across calls: %q then %q", first.CacheName, second.CacheName)
	}
}

func TestFingerprintRoundTrips(t *testing.T) {
	for _, logical := range []string{
		"/static/css/app.css",
		"/static/js/htmx.min.js", // two extensions: the hash goes before the last
		"/static/manifest.webmanifest",
	} {
		got := insertFingerprint(logical, "0123456789ab")
		clean, sum, ok := stripFingerprint(got)
		if !ok {
			t.Errorf("stripFingerprint(%q) did not recognise a fingerprint", got)
			continue
		}
		if clean != logical {
			t.Errorf("round trip of %q gave %q", logical, clean)
		}
		if sum != "0123456789ab" {
			t.Errorf("round trip of %q gave sum %q", logical, sum)
		}
	}
}

// A plain path must not be mistaken for a fingerprinted one — otherwise
// htmx.min.js would be served as immutable under a URL that does not name
// its contents.
func TestPlainPathsAreNotMistakenForFingerprints(t *testing.T) {
	for _, p := range []string{
		"/static/js/htmx.min.js",
		"/static/css/app.css",
		"/static/fonts/instrument-sans-latin-400-normal.woff2",
		"/static/js/app.notahash.js", // right shape, not hex
	} {
		if _, _, ok := stripFingerprint(p); ok {
			t.Errorf("%q was read as fingerprinted", p)
		}
	}
}

func TestAssetPathRefusesTraversal(t *testing.T) {
	for _, p := range []string{
		"/static/../../etc/passwd",
		"/static/css/../../../../etc/passwd",
	} {
		got, ok := assetPath(p)
		if ok && !strings.HasPrefix(filepath.ToSlash(got), filepath.ToSlash(assetRoot)) {
			t.Errorf("assetPath(%q) escaped the asset root: %q", p, got)
		}
	}
}

// The cache policy is the point of the exercise: content-addressed URLs
// may be kept for a year, everything else must be revalidated.
func TestCachePolicyFollowsAddressability(t *testing.T) {
	h := assetHandler(http.StripPrefix("/static/", http.FileServer(http.Dir(assetRoot))))

	fingerprinted := assets.URL("/static/css/app.css")
	if fingerprinted == "/static/css/app.css" {
		t.Fatal("app.css did not fingerprint; is web/static/css/app.css present?")
	}

	cases := []struct {
		name, path, wantCache string
	}{
		{"current fingerprint", fingerprinted, "public, max-age=31536000, immutable"},
		{"stale fingerprint", insertFingerprint("/static/css/app.css", "aaaaaaaaaaaa"), "no-store"},
		{"no fingerprint", "/static/css/app.css", "no-cache"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 (a stale fingerprint must still serve the file, not break the page)", c.path, rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != c.wantCache {
				t.Errorf("GET %s Cache-Control = %q, want %q", c.path, got, c.wantCache)
			}
			if rec.Body.Len() == 0 {
				t.Errorf("GET %s served an empty body", c.path)
			}
		})
	}
}

// TestTemplatesRouteStaticThroughAsset stops a plain /static/ URL from
// creeping back into markup. One such reference is enough to reintroduce
// the whole problem for that file, and it would look completely normal in
// review.
func TestTemplatesRouteStaticThroughAsset(t *testing.T) {
	root := repoPath("web", "templates")
	var checked int

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return nil
		}
		// The service worker template is where fingerprinted URLs are
		// rendered INTO, so it legitimately contains none of its own.
		if strings.HasSuffix(path, "sw.js.tmpl") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(raw)
		rel, _ := filepath.Rel(root, path)

		for _, attr := range []string{`href="/static/`, `src="/static/`} {
			if i := strings.Index(body, attr); i >= 0 {
				line := body[i:]
				if j := strings.IndexByte(line, '\n'); j >= 0 {
					line = line[:j]
				}
				t.Errorf("%s references a static asset directly instead of via {{asset \"...\"}}, so it will never be cache-busted:\n%s",
					rel, strings.TrimSpace(line))
			}
		}
		checked += strings.Count(body, `{{asset "`)
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}

	// The layout alone carries ten. A collapse to zero means the helper
	// stopped being used and this test silently guards nothing.
	if checked < 10 {
		t.Fatalf("found %d {{asset}} references across the templates, want at least 10", checked)
	}
	t.Logf("checked %d {{asset}} references", checked)
}
