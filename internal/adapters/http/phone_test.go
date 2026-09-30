package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	extension "github.com/mikeyaustin/jlp/chrome-extension"
)

func TestBookmarkletCarriesCaptureAndOrigin(t *testing.T) {
	b := bookmarklet("https://jlp.example.test")
	if !strings.HasPrefix(b, "javascript:") {
		t.Fatalf("not a bookmarklet: %.30s", b)
	}
	js, err := url.PathUnescape(strings.TrimPrefix(b, "javascript:"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{extension.ArticleCapture(), `"https://jlp.example.test"`, "jlp-capture-ready", "jlp-capture-article", "/reading/capture"} {
		if !strings.Contains(js, want[:min(len(want), 200)]) {
			t.Fatalf("bookmarklet missing %.60q", want)
		}
	}
	if len(b) > 64<<10 {
		t.Fatalf("bookmarklet is %d bytes", len(b))
	}
}

func TestBookmarkletOriginIsAJSStringLiteral(t *testing.T) {
	js, _ := url.PathUnescape(strings.TrimPrefix(bookmarklet(`https://x.test/"+alert(1)+"`), "javascript:"))
	if strings.Contains(js, `"https://x.test/"+alert(1)+""`) {
		t.Fatal("origin must be JSON-encoded, not concatenated raw")
	}
}

func TestExtAssetServesEmbeddedFilesImmutably(t *testing.T) {
	h := NewServer(testOptions()).HandlerForTest()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	res := get(extAssetURL("imaging.js"))
	if res.Code != 200 || !strings.Contains(res.Header().Get("Content-Type"), "javascript") ||
		!strings.Contains(res.Header().Get("Cache-Control"), "immutable") || !strings.Contains(res.Body.String(), "JLPImaging") {
		t.Fatalf("%d %v", res.Code, res.Header())
	}
	if !strings.Contains(extAssetURL("imaging.js"), "?v=") || strings.HasSuffix(extAssetURL("imaging.js"), "?v=") {
		t.Fatalf("no content hash: %s", extAssetURL("imaging.js"))
	}
	if r := get("/static/ext/popup.js"); r.Code != 404 {
		t.Fatalf("popup.js must not be served: %d", r.Code)
	}
}

func TestSettingsPhonePageShowsBookmarklet(t *testing.T) {
	h := NewServer(testOptions()).HandlerForTest()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/phone", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `href="javascript:`) || !strings.Contains(rec.Body.String(), "スマホで読解") {
		t.Fatalf("settings/phone: %d", rec.Code)
	}
}

func TestPublicOriginPrefersConfiguredURL(t *testing.T) {
	s := &Server{opts: Options{PublicURL: "https://jlp.lan.example/"}}
	r := httptest.NewRequest("GET", "http://other/settings/phone", nil)
	if got := s.publicOrigin(r); got != "https://jlp.lan.example" {
		t.Fatalf("got %q", got)
	}
	s.opts.PublicURL = ""
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Host = "jlp.fromhost.example"
	if got := s.publicOrigin(r); got != "https://jlp.fromhost.example" {
		t.Fatalf("got %q", got)
	}
}
