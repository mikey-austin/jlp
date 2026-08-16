package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// createTestSession POSTs to /sessions with the given teacher_mode and
// feedback_timing profile fields and returns the new session's ID,
// extracted from the redirect's Location header — the same
// "go through the real HTTP route, not a repository backdoor" approach
// every other handler test in this package uses.
func createTestSession(t *testing.T, h http.Handler, teacherMode, feedbackTiming string) string {
	t.Helper()
	form := url.Values{}
	form.Set("title", "会話練習")
	form.Set("purpose", "Casual conversation practice")
	form.Set("teacher_mode", teacherMode)
	form.Set("feedback_timing", feedbackTiming)
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /sessions status = %d, body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	id := strings.TrimPrefix(loc, "/sessions/")
	if id == "" || id == loc {
		t.Fatalf("Location = %q, could not extract session id", loc)
	}
	return id
}

func postConversationSay(t *testing.T, h http.Handler, sessionID, text string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	form.Set("text", text)
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/conversation", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestConversationSayImmediateTimingShowsCorrectionInline pins the
// brief's Step 4 scenario: an "immediate" session's turn response
// includes the correction card right away.
func TestConversationSayImmediateTimingShowsCorrectionInline(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	sid := createTestSession(t, h, "teacher", "immediate")

	rec := postConversationSay(t, h, sid, "昨日の映画はとても面白いでした。")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST conversation status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "面白かったです") {
		t.Fatalf("immediate-timing turn response missing the correction's replacement: %s", body)
	}
}

// TestConversationSayEndTimingWithholdsUntilSummarise pins the brief's
// mandate that "end" genuinely withholds the correction DATA from the
// turn response — not merely hides it with CSS — and that Summarise
// is what releases it.
func TestConversationSayEndTimingWithholdsUntilSummarise(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	sid := createTestSession(t, h, "teacher", "end")

	rec := postConversationSay(t, h, sid, "昨日の映画はとても面白いでした。")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST conversation status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "面白かったです") {
		t.Fatalf("end-timing turn response leaked the correction's replacement, want it withheld: %s", body)
	}

	summaryReq := httptest.NewRequest(http.MethodPost, "/sessions/"+sid+"/conversation/summary", nil)
	summaryRec := httptest.NewRecorder()
	h.ServeHTTP(summaryRec, summaryReq)
	if summaryRec.Code != http.StatusOK {
		t.Fatalf("POST conversation/summary status = %d, body=%s", summaryRec.Code, summaryRec.Body.String())
	}
	if !strings.Contains(summaryRec.Body.String(), "面白かったです") {
		t.Fatalf("digest response missing the withheld correction's replacement: %s", summaryRec.Body.String())
	}
}

// TestConversationSayCrossSessionMisses pins identity-scoping at the
// HTTP layer: posting to a session ID that doesn't exist (or belongs
// to another identity) 404s.
func TestConversationSayCrossSessionMisses(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	rec := postConversationSay(t, h, "00000000-0000-0000-0000-000000000000", "こんにちは。")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST conversation for unknown session status = %d, want 404", rec.Code)
	}
}

// TestConversationSayGatedCorrectionShowsHintNotReplacement is the
// task's mandated pin: correction.IsGated must be honoured on the
// conversation surface exactly like every other surface — a socratic
// correction's card shows the hint, and must NOT leak the replacement,
// until it's revealed (which this test never does).
func TestConversationSayGatedCorrectionShowsHintNotReplacement(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	sid := createTestSession(t, h, "socratic", "immediate")

	rec := postConversationSay(t, h, sid, "昨日の映画はとても面白いでした。")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST conversation status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "い形容詞の過去形の作り方を思い出してください") {
		t.Fatalf("gated conversation correction missing its hint: %s", body)
	}
	if strings.Contains(body, "面白かったです") {
		t.Fatalf("gated conversation correction leaked the replacement into the response: %s", body)
	}
}
