package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	"github.com/mikeyaustin/jlp/internal/application/reading"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type draftOut struct {
	DraftID        string `json:"draft_id"`
	Pages          int    `json:"pages"`
	Paragraphs     int    `json:"paragraphs"`
	KeptParagraphs int    `json:"kept_paragraphs"`
	Images         int    `json:"images"`
	Chars          int    `json:"chars"`
	ReviewURL      string `json:"review_url"`
}

const draftPartsPath = "/api/v1/reading/drafts/active/parts"

func addPart(t *testing.T, h http.Handler, pageURL, content string) draftOut {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"url": pageURL, "title": "経済対策", "content": content})
	rec := readingPostJSON(h, draftPartsPath, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("add part %s: %d %s", pageURL, rec.Code, rec.Body)
	}
	var out draftOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func del(h http.Handler, path string) *httptest.ResponseRecorder {
	return serve(h, httptest.NewRequest(http.MethodDelete, path, nil))
}

func TestAPIDraftPartsAccumulate(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	a := addPart(t, h, "https://x.jp/1", "政府は新たな経済対策をまとめた。\n\n物価高への対応が柱。")
	b := addPart(t, h, "https://x.jp/2", "ここから先は定期購読者のみ")
	if a.DraftID == "" || b.DraftID != a.DraftID || b.Pages != 2 || b.Paragraphs != 3 || b.ReviewURL != "/reading/drafts/"+a.DraftID {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	rec := get(h, "/api/v1/reading/drafts/active")
	var st draftOut
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || st.Pages != 2 || st.DraftID != a.DraftID {
		t.Fatalf("active: %d %s", rec.Code, rec.Body)
	}
}

func TestAPIDraftPartsMultipart(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	req := multipartSubmit(t, `{"url":"https://x.jp/1","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0,"caption":"会見"}]}`, smallJPEG(t))
	req.URL.Path = draftPartsPath
	rec := serve(h, req)
	var out draftOut
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Images != 1 || out.Paragraphs != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestAPIDraftErrors(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	if rec := get(h, "/api/v1/reading/drafts/active"); rec.Code != 404 {
		t.Fatalf("no draft GET = %d", rec.Code)
	}
	if rec := del(h, "/api/v1/reading/drafts/active"); rec.Code != 404 {
		t.Fatalf("no draft DELETE = %d", rec.Code)
	}
	if rec := readingPostJSON(h, draftPartsPath, `{"url":"https://x.jp/1","content":"  "}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "本文が空です") {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	for i := 0; i < 10; i++ {
		addPart(t, h, fmt.Sprintf("https://x.jp/%d", i), "政府は新たな経済対策をまとめた。")
	}
	rec := readingPostJSON(h, draftPartsPath, `{"url":"https://x.jp/11","content":"政府は新たな経済対策をまとめた。"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "下書きは10ページまでです") {
		t.Fatalf("11th page: %d %s", rec.Code, rec.Body)
	}
	if rec := del(h, "/api/v1/reading/drafts/active"); rec.Code != 204 {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	if rec := get(h, "/api/v1/reading/drafts/active"); rec.Code != 404 {
		t.Fatalf("after discard = %d", rec.Code)
	}
}

func TestReadingDraftScope(t *testing.T) {
	cases := []struct {
		scope, method, path string
		want                int
	}{
		{apitoken.ScopeReadingWrite, http.MethodPost, draftPartsPath, 200},
		{apitoken.ScopeReadingWrite, http.MethodGet, "/api/v1/reading/drafts/active", 200},
		{apitoken.ScopeReadingWrite, http.MethodDelete, "/api/v1/reading/drafts/active", 200},
		{apitoken.ScopeSessionsWrite, http.MethodPost, draftPartsPath, 403},
		{apitoken.ScopeSessionsWrite, http.MethodGet, "/api/v1/reading/drafts/active", 403},
		{apitoken.ScopeSessionsWrite, http.MethodDelete, "/api/v1/reading/drafts/active", 403},
		{apitoken.ScopeReadingWrite, http.MethodGet, "/api/v1/reading/drafts/other", 403},
	}
	for _, c := range cases {
		repo := &tokenRepoStub{hash: apitoken.Hash("jlp_tok"), identity: "dev", scopes: []string{c.scope}}
		mw := APIAuth(apitoken.New(repo), testAuth{id: learner.Identity{ID: "session-user"}}, testIdentityRepo{})
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer jlp_tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s %s with %s = %d, want %d", c.method, c.path, "", c.scope, rec.Code, c.want)
		}
	}
}

func TestDraftReviewPage(t *testing.T) {
	h, _, _ := readingTestServer(t, true)
	a := addPart(t, h, "https://x.jp/1", "政府は新たな経済対策をまとめた。\n\n物価高への対応が柱。")
	addPart(t, h, "https://x.jp/2", "ここから先は定期購読者のみ")
	rec := get(h, a.ReviewURL)
	body := rec.Body.String()
	for _, want := range []string{"1ページ目", "2ページ目", "ここから先は定期購読者のみ", "2ページ・段落 3/3・画像 0/0・", "経済対策", "Kindleに送る", "/toggle"} {
		if !strings.Contains(body, want) {
			t.Errorf("review page lacks %q", want)
		}
	}
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
}

func firstSeq(t *testing.T, body string, n int) string {
	t.Helper()
	m := regexp.MustCompile(`/blocks/(\d+)/toggle`).FindAllStringSubmatch(body, -1)
	if len(m) <= n {
		t.Fatalf("no toggle %d in %s", n, body)
	}
	return m[n][1]
}

func TestDraftToggleTitleSend(t *testing.T) {
	h, _, sender := readingTestServer(t, true)
	a := addPart(t, h, "https://x.jp/1", "政府は新たな経済対策をまとめた。\n\nここから先は定期購読者のみ")
	seq := firstSeq(t, get(h, a.ReviewURL).Body.String(), 1)

	req := httptest.NewRequest(http.MethodPost, a.ReviewURL+"/blocks/"+seq+"/toggle", nil)
	req.Header.Set("HX-Request", "true")
	rec := serve(h, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "draft-block--excluded") || !strings.Contains(body, `hx-swap-oob="true"`) || !strings.Contains(body, "段落 1/2") {
		t.Fatalf("toggle: %d %s", rec.Code, body)
	}
	treq := httptest.NewRequest(http.MethodPost, a.ReviewURL+"/title", strings.NewReader(url.Values{"title": {"新しい題"}}.Encode()))
	treq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	treq.Header.Set("HX-Request", "true")
	if rec := serve(h, treq); rec.Code != 204 {
		t.Fatalf("title = %d", rec.Code)
	}
	if !strings.Contains(get(h, a.ReviewURL).Body.String(), "新しい題") {
		t.Fatal("title not saved")
	}
	if rec := readingPostForm(h, a.ReviewURL+"/blocks/"+seq+"/toggle", nil); rec.Code != 303 {
		t.Fatalf("plain toggle = %d", rec.Code)
	}
	readingPostForm(h, a.ReviewURL+"/blocks/"+seq+"/toggle", nil)
	rec = readingPostForm(h, a.ReviewURL+"/send", url.Values{"deliver": {"on"}})
	loc := rec.Header().Get("Location")
	if rec.Code != 303 || !strings.HasPrefix(loc, "/reading/") {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	page := get(h, loc).Body.String()
	if !strings.Contains(page, "政府は新たな経済対策") || strings.Contains(page, "定期購読者") || !strings.Contains(page, "新しい題") {
		t.Fatalf("edition page: %s", page)
	}
	_ = sender
	if rec := get(h, a.ReviewURL); rec.Code != 404 {
		t.Fatalf("sent draft review = %d", rec.Code)
	}
}

func TestDraftSendAllExcludedKeepsDraft(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	a := addPart(t, h, "https://x.jp/1", "政府は新たな経済対策をまとめた。")
	seq := firstSeq(t, get(h, a.ReviewURL).Body.String(), 0)
	readingPostForm(h, a.ReviewURL+"/blocks/"+seq+"/toggle", nil)
	rec := readingPostForm(h, a.ReviewURL+"/send", url.Values{"deliver": {"on"}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "本文が空です") {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, a.ReviewURL); rec.Code != 200 {
		t.Fatalf("draft lost: %d", rec.Code)
	}
}

func TestDraftDiscard(t *testing.T) {
	h, _, _ := readingTestServer(t, false)
	a := addPart(t, h, "https://x.jp/1", "政府は新たな経済対策をまとめた。")
	rec := readingPostForm(h, a.ReviewURL+"/discard", nil)
	if rec.Code != 303 || rec.Header().Get("Location") != "/reading" {
		t.Fatalf("discard: %d %s", rec.Code, rec.Header())
	}
	rec = get(h, a.ReviewURL)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "この下書きはありません") {
		t.Fatalf("after discard: %d", rec.Code)
	}
}

func TestDraftImageRoute(t *testing.T) {
	h, svc, _ := readingTestServer(t, false)
	req := multipartSubmit(t, `{"url":"https://x.jp/1","content":"政府は新たな経済対策をまとめた。","figures":[{"after_paragraph":0}]}`, smallJPEG(t))
	req.URL.Path = draftPartsPath
	var out draftOut
	_ = json.Unmarshal(serve(h, req).Body.Bytes(), &out)
	m := regexp.MustCompile(`/images/(\d+)`).FindStringSubmatch(get(h, out.ReviewURL).Body.String())
	if m == nil {
		t.Fatal("no image in review page")
	}
	img := get(h, out.ReviewURL+"/images/"+m[1])
	if img.Code != 200 || img.Header().Get("Content-Type") != "image/jpeg" || img.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(img.Header().Get("Cache-Control"), "private") {
		t.Fatalf("image: %d %v", img.Code, img.Header())
	}
	if r := get(h, out.ReviewURL+"/images/x"); r.Code != 404 {
		t.Fatalf("bad seq = %d", r.Code)
	}
	_ = svc
}

func TestDraftOtherIdentity(t *testing.T) {
	h, svc, _ := readingTestServer(t, false)
	st, err := svc.AddDraftPage(context.Background(), "someone-else", reading.DraftPage{PageURL: "https://x.jp/1", Content: "政府は新たな経済対策をまとめた。"})
	if err != nil {
		t.Fatal(err)
	}
	base := "/reading/drafts/" + st.Draft.ID
	for _, p := range []string{base, base + "/images/0"} {
		if rec := get(h, p); rec.Code != 404 {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
	for _, p := range []string{base + "/blocks/0/toggle", base + "/title", base + "/send", base + "/discard"} {
		if rec := readingPostForm(h, p, url.Values{"title": {"x"}}); rec.Code != 404 {
			t.Errorf("POST %s = %d", p, rec.Code)
		}
	}
	if _, err := svc.ActiveDraft(context.Background(), "someone-else"); err != nil {
		t.Fatalf("foreign draft was touched: %v", err)
	}
}
