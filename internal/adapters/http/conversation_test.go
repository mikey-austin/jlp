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

// TestConversationSayCleanEndTimingTurnNotCaptionedWithheld pins
// Finding I-4 at the layer the bug actually lived in
// (toConversationTurnView's Withheld derivation,
// internal/adapters/http/conversation.go): a genuinely clean message
// under "end" timing must render with NO withheld caption, while a
// message that actually found (and is withholding) a correction must
// still show one. The report's own regression test
// (application/conversation/service_test.go's Pending==0 assertion) pins
// one layer below this — see the report's Finding I-4 entry for why
// that alone doesn't protect this HTTP-level rendering decision.
func TestConversationSayCleanEndTimingTurnNotCaptionedWithheld(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	sid := createTestSession(t, h, "teacher", "end")

	clean := postConversationSay(t, h, sid, "今日はいい天気ですね。")
	if clean.Code != http.StatusOK {
		t.Fatalf("POST conversation (clean) status = %d, body=%s", clean.Code, clean.Body.String())
	}
	if strings.Contains(clean.Body.String(), "訂正はまだ表示されていません") {
		t.Fatalf("clean end-timing turn was captioned as withheld, but nothing was ever found: %s", clean.Body.String())
	}

	dirty := postConversationSay(t, h, sid, "昨日の映画はとても面白いでした。")
	if dirty.Code != http.StatusOK {
		t.Fatalf("POST conversation (dirty) status = %d, body=%s", dirty.Code, dirty.Body.String())
	}
	if !strings.Contains(dirty.Body.String(), "訂正はまだ表示されていません") {
		t.Fatalf("end-timing turn with an actual withheld correction is missing its caption: %s", dirty.Body.String())
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

// TestConversationSayGatedCorrectionHasNoInteractiveControls pins
// Finding I-2: a gated conversation correction's card must not render
// retry/reveal <form>s — they'd 404 in production (no corrections-table
// row backs a conversation correction's ID; see
// internal/adapters/http/feedback.go's RevealCorrection/RetryCorrection
// handlers). With this session's timing at "end", the digest is the
// ONLY surface a socratic conversation correction reaches, so a
// guaranteed-404 button there is a functional dead end, not cosmetic.
func TestConversationSayGatedCorrectionHasNoInteractiveControls(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	sid := createTestSession(t, h, "socratic", "immediate")

	rec := postConversationSay(t, h, sid, "昨日の映画はとても面白いでした。")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST conversation status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "/retry") {
		t.Fatalf("gated conversation correction rendered a retry form pointing at a 404ing route: %s", body)
	}
	if strings.Contains(body, "/reveal") {
		t.Fatalf("gated conversation correction rendered a reveal form pointing at a 404ing route: %s", body)
	}
}

// TestConversationGatedTurnCardSaysWhereTheAnswerComesFrom pins half of
// whole-branch review C-2: a gated conversation card has no reveal
// control of its own (see the test above), so if it also says nothing
// about where the answer comes from it is a hint the learner can never
// resolve — the exact dead end the Task 6 fix removed a 404ing button
// for, one layer up. The card must name the surface that reveals.
func TestConversationGatedTurnCardSaysWhereTheAnswerComesFrom(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	sid := createTestSession(t, h, "socratic", "immediate")

	rec := postConversationSay(t, h, sid, "昨日の映画はとても面白いでした。")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST conversation status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, conversationGatedNote) {
		t.Fatalf("gated conversation card gives the learner no route to the answer (expected %q): %s", conversationGatedNote, body)
	}
}

// TestConversationSummariseRevealsTheAnswerToAGatedCorrection is the
// other half of C-2, and it deliberately REPLACES an earlier test that
// pinned the opposite (the digest re-gating). That test pinned a
// permanent dead end: a conversation correction has no corrections-table
// row, so there is no per-correction reveal route and never was one, and
// the digest is the last surface such a correction ever reaches. With
// the digest gating too, a learner in the DEFAULT configuration
// (socratic + "end" timing) could be shown a hint whose answer existed
// on no surface at all.
//
// The ruling (see toConversationDigestCardView) is that pressing
// 会話をまとめる IS the conversation's reveal — a deliberate learner
// action, after the conversation where the socratic eliciting actually
// happens. This test pins that the answer arrives, and the test above
// pins that nothing leaks before the learner asks.
func TestConversationSummariseRevealsTheAnswerToAGatedCorrection(t *testing.T) {
	h := NewServer(testOptionsWithSessions()).HandlerForTest()
	sid := createTestSession(t, h, "socratic", "end")

	rec := postConversationSay(t, h, sid, "昨日の映画はとても面白いでした。")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST conversation status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Precondition: the turn itself withheld, so the digest is genuinely
	// this correction's first and only sight of the answer.
	if strings.Contains(rec.Body.String(), "面白かったです") {
		t.Fatalf("precondition failed: the turn surface already showed the replacement: %s", rec.Body.String())
	}

	summaryReq := httptest.NewRequest(http.MethodPost, "/sessions/"+sid+"/conversation/summary", nil)
	summaryRec := httptest.NewRecorder()
	h.ServeHTTP(summaryRec, summaryReq)
	if summaryRec.Code != http.StatusOK {
		t.Fatalf("POST conversation/summary status = %d, body=%s", summaryRec.Code, summaryRec.Body.String())
	}
	body := summaryRec.Body.String()
	if !strings.Contains(body, "面白かったです") {
		t.Fatalf("digest withheld the replacement — a socratic conversation correction's answer is reachable on NO surface: %s", body)
	}
	if !strings.Contains(body, "い形容詞の過去形は") {
		t.Fatalf("digest withheld the explanation: %s", body)
	}
	// And no dead affordance: the digest card must not offer controls
	// that post to routes a conversation correction has no row behind.
	for _, dead := range []string{"/retry", "/reveal", "/status"} {
		if strings.Contains(body, dead) {
			t.Fatalf("digest card rendered a control pointing at %s, which 404s for a conversation correction: %s", dead, body)
		}
	}
	// Nor the internal lifecycle sentinel: "noted" is a value this
	// adapter invents for a card with no lifecycle, not a word for the
	// learner — and every digest card now goes through that branch.
	if strings.Contains(body, `class="resolved"`) {
		t.Errorf("digest card printed its internal status sentinel as a footer: %s", body)
	}
}

// TestCorrectionCardRendersTheComputedGateNotItsOwnPredicate pins the
// E1/F1 collapse: correction_card.html.tmpl must branch on the
// PRE-COMPUTED correctionCardView.Gated field and hold no copy of the
// socratic predicate itself. It used to spell out
// `and .HasHint (eq .Status "presented") (not .Revealed)` — a fourth
// copy of domain correction.IsGated, and, since Phase 4 Task 6, the
// single gate for three surfaces (writing pane, conversation turns,
// conversation digest) whose view builders both copy Replacement and
// Explanation into the view unconditionally.
//
// Both directions are asserted with deliberately CONTRADICTORY inputs —
// a view whose raw fields say one thing and whose Gated says the other —
// because that is the only way to tell "the template consumed the
// computed field" apart from "the template happened to agree". Against
// the old template each half fails.
func TestCorrectionCardRendersTheComputedGateNotItsOwnPredicate(t *testing.T) {
	render := func(t *testing.T, v correctionCardView) string {
		t.Helper()
		rec := httptest.NewRecorder()
		RenderPartial(rec, httptest.NewRequest(http.MethodGet, "/", nil), "correction_card", v)
		return rec.Body.String()
	}

	// Gated true, but the raw fields the old inline predicate read say
	// "not gated" (Status is not "presented", and Revealed is true).
	withheld := render(t, correctionCardView{
		ID: "c1", Original: "面白いでした", Replacement: "面白かったです",
		ExplanationJA: "説明", HintJA: "ヒント",
		Status: "accepted", HasHint: true, Revealed: true, Gated: true,
	})
	if strings.Contains(withheld, "面白かったです") {
		t.Fatalf("card ignored Gated=true and re-derived the gate from Status/Revealed, leaking the replacement: %s", withheld)
	}
	if !strings.Contains(withheld, "ヒント") {
		t.Fatalf("gated card did not render its hint: %s", withheld)
	}

	// Gated false, but the raw fields say "gated".
	shown := render(t, correctionCardView{
		ID: "c2", Original: "面白いでした", Replacement: "面白かったです",
		ExplanationJA: "説明", HintJA: "ヒント",
		Status: "noted", HasHint: true, Revealed: false, Gated: false,
	})
	if !strings.Contains(shown, "面白かったです") {
		t.Fatalf("card ignored Gated=false and re-derived the gate from HasHint/Status/Revealed, withholding the replacement: %s", shown)
	}
}
