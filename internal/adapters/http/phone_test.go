package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

func TestReadingCapturePageIsNotAnEdition(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reading/capture", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "/static/js/reading-capture.") || !strings.Contains(body, "imaging.js") {
		t.Fatalf("reading/capture: %d", rec.Code)
	}
}

func TestManifestDeclaresShareTarget(t *testing.T) {
	b, err := os.ReadFile(repoPath("web", "static", "manifest.webmanifest"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ShareTarget struct {
			Action, Method, Enctype string
			Params                  map[string]string
		} `json:"share_target"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	st := m.ShareTarget
	if st.Action != "/reading/share" || st.Method != "POST" || st.Enctype != "application/x-www-form-urlencoded" ||
		st.Params["title"] != "title" || st.Params["text"] != "text" || st.Params["url"] != "url" {
		t.Fatalf("share_target = %+v", st)
	}
}

func TestServiceWorkerHandlesSharesAndKeepsTheirCache(t *testing.T) {
	h := NewServer(testOptions()).HandlerForTest()
	body := get(h, "/static/sw.js").Body.String()
	for _, want := range []string{`"/reading/share"`, `"jlp-share"`, "303", "name !== SHARE_CACHE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("sw.js missing %s", want)
		}
	}
}

func TestReadingSharePages(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	if rec := get(h, "/reading/share?pending=x"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "reading-share.") {
		t.Fatalf("GET /reading/share: %d", rec.Code)
	}

	form := url.Values{"title": {"共有記事"}, "text": {"本文です。"}, "url": {"https://example.test/a"}}
	req := httptest.NewRequest(http.MethodPost, "/reading/share", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "もう一度共有") || strings.Contains(rec.Body.String(), "reading-share.") {
		t.Fatalf("POST /reading/share: %d", rec.Code)
	}
	if list := get(h, "/reading"); strings.Contains(list.Body.String(), "共有記事") {
		t.Fatal("a share POST the worker missed must create nothing")
	}
}
