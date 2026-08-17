package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	agentconversation "github.com/mikeyaustin/jlp/internal/agent/conversation"
	agentlesson "github.com/mikeyaustin/jlp/internal/agent/lesson"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	appfeedback "github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Soft delete at the HTTP layer (Phase 4 Task D).
//
// The centrepiece is TestDeletedContentReachesNoReadSurface: rather
// than asserting the delete route returned 303 and calling it proven,
// it enumerates every page and JSON endpoint that can show a session, a
// word or a lesson, drives all of them, and fails if a deleted thing
// turns up on any one. The point is that the list of surfaces is
// written down and driven, so adding a surface without the filter is
// something a test can catch rather than something a reviewer has to
// notice.
//
// Surfaces reachable from this package are all covered here. Two are
// not, and are covered in internal/adapters/postgres/softdelete_
// integration_test.go instead, against a real database: the repository
// reads behind the agent tool registry (get_active_session,
// search_vocabulary, get_correction_history, …), which are pure
// pass-throughs to the same service methods driven below, and the
// statistics that must NOT move (/learner, /outcomes), which this
// package can only see through a fixed-value fake.

// softDeleteFixture is one server with all three deletable kinds wired
// up over in-memory repositories that honour deleted_at exactly as the
// SQL does, plus one of each kind owned by SOMEONE ELSE — so every test
// here can check the cross-identity contract without extra setup.
type softDeleteFixture struct {
	h        http.Handler
	sessions *fakeSessionRepo
	vocab    *fakeVocabRepo
	lessons  *fakeLessonRepo

	sessionID session.ID
	itemID    string
	lessonID  string

	// The other identity's rows. testAuth authenticates every request
	// as "dev", so these are only ever reachable by forging an id in
	// the URL — which is exactly what the authorization tests do.
	otherSessionID session.ID
	otherItemID    string
	otherLessonID  string
}

const otherLearner = learner.IdentityID("someone-else")

func newSoftDeleteFixture(t *testing.T) *softDeleteFixture {
	t.Helper()
	opts := testOptions()
	events := newFakeEventRepo()
	rec := learning.NewRecorder(events, inprocbus.New())

	sessRepo := newFakeSessionRepo()
	vocabRepo := newFakeVocabRepo()
	lessonRepo := newFakeLessonRepo()

	docRepo := newFakeDocRepo()
	opts.Sessions = sessions.NewService(sessRepo, rec)
	opts.Vocabulary = appvocabulary.NewService(vocabRepo, rec)
	opts.Writing = appwriting.NewService(docRepo, rec)
	opts.Events = events
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{})

	teachingPlanner := planner.NewPlanner(&fakeObservationRepo{}, events, fakeGrammarRepo{}, fakePriorityRepo{}, vocabRepo, time.Now)
	feedbackRepo := newFakeFeedbackRepo()
	opts.Lessons = applessons.NewService(lessonRepo, fakePriorityRepo{}, teachingPlanner, feedbackRepo, &fakeObservationRepo{}, agentlesson.New(fakeai.New()), rec)
	opts.LessonsRepo = lessonRepo

	// sessionsWorkspace reads Conversation.History and
	// Feedback.ListForSession on every render, so the workspace route
	// needs both wired over the SAME sessRepo/docRepo the delete path
	// uses — otherwise "the workspace 404s after a delete" would pass
	// for the wrong reason.
	opts.Conversation = appconversation.NewService(newFakeConversationRepo(), sessRepo, agentconversation.New(fakeai.New()), nil, rec, fakeGrammarRepo{}, events)
	opts.Feedback = appfeedback.NewService(sessRepo, docRepo, feedbackRepo, fakeGrammarRepo{}, fakePriorityRepo{}, teachingPlanner, appvocabulary.NewService(vocabRepo, rec), teacher.New(fakeai.New()), rec, false, nil, nil)

	f := &softDeleteFixture{h: NewServer(opts).HandlerForTest(), sessions: sessRepo, vocab: vocabRepo, lessons: lessonRepo}

	f.sessionID = f.seedSession(t, testAuthIdentity, "日記の練習")
	f.otherSessionID = f.seedSession(t, otherLearner, "誰かのセッション")
	f.itemID = f.seedItem(t, testAuthIdentity, "取り組む")
	f.otherItemID = f.seedItem(t, otherLearner, "誰かの言葉")
	f.lessonID = f.seedLesson(t, testAuthIdentity)
	f.otherLessonID = f.seedLesson(t, otherLearner)

	return f
}

// Every seeded id is a real UUID, matching what the application layer
// generates — the delete/restore routes and the ?undo= parameter both
// treat ids as UUIDs, so a fixture using readable slugs would test a
// shape production never sees.
func (f *softDeleteFixture) seedSession(t *testing.T, identity learner.IdentityID, title string) session.ID {
	t.Helper()
	id := session.ID(uuid.NewString())
	s := session.Session{ID: id, IdentityID: identity, Title: title, Purpose: "Diary", UpdatedAt: time.Now()}
	if err := f.sessions.Create(context.Background(), s); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

func (f *softDeleteFixture) seedItem(t *testing.T, identity learner.IdentityID, expression string) string {
	t.Helper()
	item := &vocabulary.Item{
		ID:         uuid.NewString(),
		IdentityID: identity,
		Expression: expression,
		Meaning:    "meaning of " + expression,
		Kind:       vocabulary.KindWord,
		Lookups:    1,
		FirstSeen:  time.Now(),
		LastEvent:  time.Now(),
	}
	f.vocab.items[vocabKey(identity, expression)] = item
	f.vocab.byID[item.ID] = item
	return item.ID
}

func (f *softDeleteFixture) seedLesson(t *testing.T, identity learner.IdentityID) string {
	t.Helper()
	id := uuid.NewString()
	l := storage.Lesson{ID: id, IdentityID: identity, Plan: []byte(`{"level_summary":"s"}`), Status: "prepared", CreatedAt: time.Now()}
	if err := f.lessons.Insert(context.Background(), l); err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	return id
}

func (f *softDeleteFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// testAuthIdentity is the identity every request in this package is
// authenticated as (see testOptions' testAuth).
const testAuthIdentity = learner.IdentityID("dev")

// readSurfaces enumerates every GET this package can drive that could
// show a session, a word or a lesson. Deliberately a written-down list:
// a new surface added without the repository filter shows up here as a
// failing sweep rather than as something a reviewer had to spot.
func readSurfaces() []string {
	return []string{
		"/",                // home dashboard: recent sessions
		"/sessions",        // the session list
		"/api/v1/sessions", // the same list over JSON
		"/vocabulary",      // すべて
		"/vocabulary?filter=looked-up",
		"/vocabulary?filter=produced",
		"/vocabulary?filter=activate",
		"/lessons", // the lesson list
	}
}

func TestDeletedContentReachesNoReadSurface(t *testing.T) {
	f := newSoftDeleteFixture(t)

	// Everything is visible first, so a sweep that passes because the
	// fixture never rendered anything cannot masquerade as a pass.
	if body := f.get(t, "/sessions").Body.String(); !strings.Contains(body, "日記の練習") {
		t.Fatal("precondition: the session is not on /sessions before the delete")
	}
	if body := f.get(t, "/vocabulary").Body.String(); !strings.Contains(body, "取り組む") {
		t.Fatal("precondition: the word is not on /vocabulary before the delete")
	}
	if body := f.get(t, "/lessons").Body.String(); !strings.Contains(body, f.lessonID) {
		t.Fatal("precondition: the lesson is not on /lessons before the delete")
	}

	for path, want := range map[string]int{
		"/sessions/" + string(f.sessionID) + "/delete": http.StatusSeeOther,
		"/vocabulary/" + f.itemID + "/delete":          http.StatusSeeOther,
		"/lessons/" + f.lessonID + "/delete":           http.StatusSeeOther,
	} {
		if got := postForm(t, f.h, path, url.Values{}).Code; got != want {
			t.Fatalf("POST %s status = %d, want %d", path, got, want)
		}
	}

	// The needles: the exact strings a leaked row would put on a page.
	// The vocabulary meaning is included because a template could stop
	// rendering the expression yet still render its definition.
	needles := []string{"日記の練習", string(f.sessionID), "取り組む", "meaning of 取り組む", f.itemID, f.lessonID}

	for _, path := range readSurfaces() {
		rec := f.get(t, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 — an unreachable surface proves nothing", path, rec.Code)
			continue
		}
		body := rec.Body.String()
		for _, needle := range needles {
			if strings.Contains(body, needle) {
				t.Errorf("GET %s leaks soft-deleted content: found %q", path, needle)
			}
		}
	}

	// The by-id surfaces answer 404, the same as an id that never
	// existed — a deleted session's workspace must not stay open.
	for _, path := range []string{
		"/sessions/" + string(f.sessionID),
		"/api/v1/sessions/" + string(f.sessionID),
		"/lessons/" + f.lessonID,
	} {
		if got := f.get(t, path).Code; got != http.StatusNotFound {
			t.Errorf("GET %s after delete = %d, want 404", path, got)
		}
	}
}

// The JSON API is a separate consumer of the same services; asserting
// on the decoded array rather than on substrings makes "the session is
// absent" mean absent, not merely differently spelled.
func TestDeletedSessionIsAbsentFromTheJSONAPI(t *testing.T) {
	f := newSoftDeleteFixture(t)

	if got := postForm(t, f.h, "/sessions/"+string(f.sessionID)+"/delete", url.Values{}).Code; got != http.StatusSeeOther {
		t.Fatalf("delete status = %d", got)
	}

	rec := f.get(t, "/api/v1/sessions")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/sessions status = %d", rec.Code)
	}
	var payload []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	for _, s := range payload {
		if s.ID == string(f.sessionID) {
			t.Fatal("/api/v1/sessions still lists the deleted session")
		}
	}
}

// --- Authorization ------------------------------------------------------
//
// The user's explicit ask: "make sure authorization is always checked
// according to the user in session and attribution." Each of these
// asserts BOTH halves. A 404-only assertion would pass even if the
// handler had deleted the other identity's row on its way to answering
// 404, which is the failure that actually matters.

func TestDeletingAnotherIdentitysSessionMissesAndLeavesItIntact(t *testing.T) {
	f := newSoftDeleteFixture(t)

	rec := postForm(t, f.h, "/sessions/"+string(f.otherSessionID)+"/delete", url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-identity delete status = %d, want 404 (indistinguishable from an unknown id)", rec.Code)
	}
	if !f.sessions.exists(otherLearner, f.otherSessionID) {
		t.Fatal("the other identity's session row was destroyed")
	}
	if f.sessions.isDeleted(otherLearner, f.otherSessionID) {
		t.Fatal("the other identity's session was marked deleted by a request authenticated as someone else")
	}
}

func TestDeletingAnotherIdentitysWordMissesAndLeavesItIntact(t *testing.T) {
	f := newSoftDeleteFixture(t)

	rec := postForm(t, f.h, "/vocabulary/"+f.otherItemID+"/delete", url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-identity delete status = %d, want 404", rec.Code)
	}
	if !f.vocab.exists(f.otherItemID) {
		t.Fatal("the other identity's vocabulary row was destroyed")
	}
	if f.vocab.isDeleted(f.otherItemID) {
		t.Fatal("the other identity's word was marked deleted by a request authenticated as someone else")
	}
}

func TestDeletingAnotherIdentitysLessonMissesAndLeavesItIntact(t *testing.T) {
	f := newSoftDeleteFixture(t)

	rec := postForm(t, f.h, "/lessons/"+f.otherLessonID+"/delete", url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-identity delete status = %d, want 404", rec.Code)
	}
	if !f.lessons.exists(f.otherLessonID) {
		t.Fatal("the other identity's lesson row was destroyed")
	}
	if f.lessons.isDeleted(f.otherLessonID) {
		t.Fatal("the other identity's lesson was marked deleted by a request authenticated as someone else")
	}
}

// A deleted thing that never existed and a deleted thing belonging to
// someone else must be indistinguishable — no existence oracle. Both
// answer with the same status and the same body.
func TestUnknownAndForeignIDsAreIndistinguishable(t *testing.T) {
	f := newSoftDeleteFixture(t)

	foreign := postForm(t, f.h, "/sessions/"+string(f.otherSessionID)+"/delete", url.Values{})
	unknown := postForm(t, f.h, "/sessions/no-such-session-at-all/delete", url.Values{})

	if foreign.Code != unknown.Code {
		t.Fatalf("foreign id status %d != unknown id status %d — the difference is an existence oracle", foreign.Code, unknown.Code)
	}
	if foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("foreign id body %q != unknown id body %q — the difference is an existence oracle", foreign.Body.String(), unknown.Body.String())
	}
}

// The identity is taken from the request context, never from the
// request. A body or query parameter claiming to be someone else must
// change nothing about who the delete applies to.
func TestIdentityInTheRequestBodyIsIgnored(t *testing.T) {
	f := newSoftDeleteFixture(t)

	form := url.Values{}
	form.Set("identity", string(otherLearner))
	form.Set("identity_id", string(otherLearner))
	rec := postForm(t, f.h, "/sessions/"+string(f.otherSessionID)+"/delete?identity="+string(otherLearner), form)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a forged identity must not widen what this request can delete", rec.Code)
	}
	if f.sessions.isDeleted(otherLearner, f.otherSessionID) {
		t.Fatal("a claimed identity in the form/query was honoured; identity must come from the request context alone")
	}
}

// --- Confirmation and undo ---------------------------------------------

// A delete control that fires on a single click, in an app used daily,
// eventually destroys work. Every list that offers one must ship the
// confirmation dialog and wire its buttons to it.
func TestListPagesShipTheDeleteConfirmationDialog(t *testing.T) {
	f := newSoftDeleteFixture(t)

	for _, path := range []string{"/sessions", "/vocabulary", "/lessons"} {
		body := f.get(t, path).Body.String()
		if !strings.Contains(body, `id="confirm-delete-modal"`) {
			t.Errorf("GET %s has a delete control but no confirmation dialog", path)
		}
		if !strings.Contains(body, "data-confirm-delete=") {
			t.Errorf("GET %s has no delete trigger wired to the confirmation dialog", path)
		}
		// The trigger must be a button, not a bare form submit: a form
		// would bypass the dialog entirely when JS is unavailable, which
		// is the one case where deleting silently would be worst.
		//
		// Only type="button" is asserted, not the full class attribute.
		// This test pinned the whole class string once, which meant
		// restyling the trigger failed a test about dialog wiring — it
		// claimed a behaviour and enforced an appearance. How the trigger
		// *looks* is TestDeleteTriggersLookDangerous's business.
		at := strings.Index(body, "data-confirm-delete=")
		if at < 0 {
			continue // already reported above
		}
		start := strings.LastIndex(body[:at], "<")
		end := strings.Index(body[at:], ">")
		if start < 0 || end < 0 {
			t.Errorf("GET %s: could not bound the delete trigger tag", path)
		} else if tag := body[start : at+end]; !strings.Contains(tag, `type="button"`) {
			t.Errorf("GET %s delete trigger is not a type=\"button\" (it would bypass the dialog without JS):\n%s", path, tag)
		}
	}
}

func TestDeleteOffersUndoAndRestoreBringsItBack(t *testing.T) {
	f := newSoftDeleteFixture(t)

	rec := postForm(t, f.h, "/sessions/"+string(f.sessionID)+"/delete", url.Values{})
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "undo="+url.QueryEscape(string(f.sessionID))) {
		t.Fatalf("delete redirected to %q, want an ?undo=<id> so the list can offer the way back", loc)
	}

	list := f.get(t, loc)
	restoreAction := "/sessions/" + string(f.sessionID) + "/restore"
	if !strings.Contains(list.Body.String(), restoreAction) {
		t.Fatalf("the list after a delete does not offer %s", restoreAction)
	}

	if got := postForm(t, f.h, restoreAction, url.Values{}).Code; got != http.StatusSeeOther {
		t.Fatalf("restore status = %d, want 303", got)
	}
	if body := f.get(t, "/sessions").Body.String(); !strings.Contains(body, "日記の練習") {
		t.Fatal("the session did not come back after restore")
	}
	if got := f.get(t, "/sessions/"+string(f.sessionID)).Code; got != http.StatusOK {
		t.Fatalf("the restored session's workspace = %d, want 200", got)
	}
}

// The ?undo= parameter only ever becomes the middle of a path this
// handler builds, and only after parsing as a UUID — a hand-crafted
// value must not be able to point the undo button anywhere.
func TestUndoParameterIsNotReflectedWhenItIsNotAnID(t *testing.T) {
	f := newSoftDeleteFixture(t)

	body := f.get(t, "/sessions?undo=https://evil.example.com/take-over").Body.String()
	if strings.Contains(body, "evil.example.com") {
		t.Fatal("a non-UUID ?undo= value reached the page")
	}
	if strings.Contains(body, "削除を取り消す") {
		t.Fatal("a non-UUID ?undo= value still rendered an undo affordance")
	}
}

func TestDeleteIsIdempotentOverHTTP(t *testing.T) {
	f := newSoftDeleteFixture(t)
	path := "/sessions/" + string(f.sessionID) + "/delete"

	if got := postForm(t, f.h, path, url.Values{}).Code; got != http.StatusSeeOther {
		t.Fatalf("first delete status = %d, want 303", got)
	}
	if got := postForm(t, f.h, path, url.Values{}).Code; got != http.StatusSeeOther {
		t.Fatalf("second delete status = %d, want 303 — deleting an already-deleted session is a success", got)
	}
}
