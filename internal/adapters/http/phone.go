// The phone paths into 読解 (spec: 読解 from the phone). The bookmarklet
// runs the extension's own capture (package extension) inside the
// article's page — where the learner is signed in — and hands the result
// to /reading/capture by postMessage; the server never fetches the page.
package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	extension "github.com/mikeyaustin/jlp/chrome-extension"
)

// bookmarkletTemplate is the handoff around the inlined capture. %s are,
// in order: the JSON-encoded JLP origin, and the capture expression.
const bookmarkletTemplate = `(()=>{const JLP=%s;let a;try{a=%s;}catch(e){a=null}
if(!a){location.href=JLP+"/reading/capture#failed";return}
const w=window.open(JLP+"/reading/capture","_blank");
if(!w){location.href=JLP+"/reading/capture#blocked";return}
const on=(e)=>{if(e.source!==w||e.origin!==JLP||!e.data||e.data.type!=="jlp-capture-ready")return;
w.postMessage({type:"jlp-capture-article",v:1,article:a},JLP);removeEventListener("message",on)};
addEventListener("message",on);setTimeout(()=>removeEventListener("message",on),300000)})()`

func bookmarklet(origin string) string {
	o, _ := json.Marshal(origin)
	js := fmt.Sprintf(bookmarkletTemplate, o, extension.ArticleCapture())
	// PathEscape keeps the URL a single bookmark-safe string; Chrome
	// decodes it before running.
	return "javascript:" + url.PathEscape(js)
}

func (s *Server) publicOrigin(r *http.Request) string {
	if u := strings.TrimRight(s.opts.PublicURL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// extHashes versions the embedded files' URLs by content, so the
// immutable cache policy below is safe.
var extHashes = map[string]string{}

func init() {
	for _, name := range []string{"article.js", "imaging.js"} {
		b, _ := extension.File(name)
		sum := sha256.Sum256(b)
		extHashes[name] = hex.EncodeToString(sum[:])[:12]
	}
	funcs["extAsset"] = extAssetURL
}

func extAssetURL(name string) string { return "/static/ext/" + name + "?v=" + extHashes[name] }

func (s *Server) extAsset(w http.ResponseWriter, r *http.Request) {
	b, ok := extension.File(chi.URLParam(r, "name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

func (s *Server) settingsPhonePage(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	s.render(w, r, "settings_phone", map[string]any{
		"Title":       "スマホで読解",
		"Identity":    ident,
		"Bookmarklet": template.URL(bookmarklet(s.publicOrigin(r))),
	})
}
