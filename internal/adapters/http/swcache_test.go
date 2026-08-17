package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The service worker serves /static/* cache-first and only ever refreshes
// a precached file when CACHE_NAME changes, so an asset edit without a
// bump ships new markup to returning learners against their old
// stylesheet and scripts. That has now happened twice: once leaving the
// 削除 buttons inert (the v4 comment in sw.js), and once leaving the
// logout button unstyled after .icon-btn was introduced.
//
// Both were caught by eye, late, in the browser. This pins it instead:
// the digest below covers every precached asset, so changing one without
// bumping CACHE_NAME fails here.
const precacheDigestFile = "testdata/precache.digest"

func TestServiceWorkerCacheNameTracksPrecachedAssets(t *testing.T) {
	swPath := repoPath("web", "static", "sw.js")
	raw, err := os.ReadFile(swPath)
	if err != nil {
		t.Fatalf("read sw.js: %v", err)
	}
	sw := string(raw)

	cacheName := parseCacheName(t, sw)
	urls := parsePrecacheURLs(t, sw)
	if len(urls) < 2 {
		t.Fatalf("parsed %d precache URLs from sw.js, which cannot be right", len(urls))
	}

	// Hash path+content of every precached file, in listed order, so
	// both edits and reorderings register.
	h := sha256.New()
	var hashed int
	for _, u := range urls {
		// "/offline" is a server-rendered route, not a file on disk; its
		// markup is covered by the handler tests.
		if !strings.HasPrefix(u, "/static/") {
			continue
		}
		p := repoPath(append([]string{"web"}, strings.Split(strings.TrimPrefix(u, "/"), "/")...)...)
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("precached asset %s is listed in sw.js but unreadable: %v", u, err)
		}
		fmt.Fprintf(h, "%s\n", u)
		h.Write(body)
		hashed++
	}
	digest := hex.EncodeToString(h.Sum(nil))

	want := fmt.Sprintf("cache_name: %s\ndigest: %s\n", cacheName, digest)
	// repoPath, not a bare "testdata/…": go test runs with the working
	// directory set to the module root in this checkout, so a relative
	// path here would read out of the worktree entirely.
	recorded, err := os.ReadFile(repoPath("internal", "adapters", "http", precacheDigestFile))
	if err != nil {
		t.Fatalf("read %s: %v\n\nwrite this file with:\n%s", precacheDigestFile, err, want)
	}

	if string(recorded) != want {
		gotName := parseField(string(recorded), "cache_name:")
		if gotName == cacheName {
			t.Fatalf(`a precached asset changed but CACHE_NAME is still %q.

Returning learners keep the old copy of every /static/ file until the
name changes, so they would get this new markup against the old CSS/JS.

Fix: bump CACHE_NAME in web/static/sw.js (with a note saying why), then
update %s to:

%s`, cacheName, precacheDigestFile, want)
		}
		t.Fatalf("CACHE_NAME is %q and the precached assets changed; update %s to:\n\n%s",
			cacheName, precacheDigestFile, want)
	}
	t.Logf("%s covers %d precached assets", cacheName, hashed)
}

func parseCacheName(t *testing.T, sw string) string {
	t.Helper()
	const marker = `const CACHE_NAME = "`
	i := strings.Index(sw, marker)
	if i < 0 {
		t.Fatal("sw.js has no `const CACHE_NAME = \"...\"` — this guard can no longer see the cache version")
	}
	rest := sw[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("sw.js CACHE_NAME is unterminated")
	}
	return rest[:j]
}

func parsePrecacheURLs(t *testing.T, sw string) []string {
	t.Helper()
	const marker = "PRECACHE_URLS = ["
	i := strings.Index(sw, marker)
	if i < 0 {
		t.Fatal("sw.js has no PRECACHE_URLS array — this guard can no longer see what is cached")
	}
	rest := sw[i+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatal("sw.js PRECACHE_URLS is unterminated")
	}
	var urls []string
	for _, line := range strings.Split(rest[:end], "\n") {
		a := strings.Index(line, `"`)
		if a < 0 {
			continue
		}
		b := strings.Index(line[a+1:], `"`)
		if b < 0 {
			continue
		}
		urls = append(urls, line[a+1:a+1+b])
	}
	return urls
}

func parseField(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(line, key))
		}
	}
	return ""
}
