package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/session"
)

func TestDocumentsSaveReturnsVersionSavedAtAndRunes(t *testing.T) {
	opts := testOptionsWithSessions()
	svc := opts.Sessions
	sess, err := svc.Create(context.Background(), "dev", "旅行について書く", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := opts.Writing.Open(context.Background(), "dev", sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(opts) // testOptions() authenticates as identity "dev"
	h := srv.HandlerForTest()

	form := url.Values{}
	form.Set("content", "昨日、映画を見た。")
	req := httptest.NewRequest(http.MethodPost, "/documents/"+string(doc.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Version int    `json:"version"`
		SavedAt string `json:"saved_at"`
		Runes   int    `json:"runes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response %s: %v", rec.Body.String(), err)
	}
	if got.Version != 2 {
		t.Fatalf("version = %d, want 2 (fresh doc is version 1, one Save bumps it)", got.Version)
	}
	if got.Runes != 9 {
		t.Fatalf("runes = %d, want 9 (rune count, not byte length)", got.Runes)
	}
	if got.SavedAt == "" {
		t.Fatal("saved_at is empty")
	}
}

// TestDocumentsSaveOversizedBodyReturns400 proves the autosave handler's
// http.MaxBytesReader cap: a body over 1MiB must fail as an ordinary
// 400 (ParseForm's error path), not hang buffering an unbounded body
// into memory or panic.
func TestDocumentsSaveOversizedBodyReturns400(t *testing.T) {
	opts := testOptionsWithSessions()
	svc := opts.Sessions
	sess, err := svc.Create(context.Background(), "dev", "旅行について書く", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := opts.Writing.Open(context.Background(), "dev", sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(opts)
	h := srv.HandlerForTest()

	form := url.Values{}
	form.Set("content", strings.Repeat("a", (1<<20)+1))
	req := httptest.NewRequest(http.MethodPost, "/documents/"+string(doc.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body len=%d", rec.Code, rec.Body.Len())
	}
}

func TestDocumentsSaveCrossIdentityNotFound(t *testing.T) {
	opts := testOptionsWithSessions()
	sess, err := opts.Sessions.Create(context.Background(), "someone-else", "他人のセッション", "Diary", session.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := opts.Writing.Open(context.Background(), "someone-else", sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	srv := NewServer(opts) // testOptions() authenticates as identity "dev"
	h := srv.HandlerForTest()

	form := url.Values{}
	form.Set("content", "mallory's text")
	req := httptest.NewRequest(http.MethodPost, "/documents/"+string(doc.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
