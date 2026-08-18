package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func agentsTestServer(t *testing.T) *Server {
	t.Helper()
	opts, _ := settingsTestOptions(t)
	opts.PromptNames = []string{"teacher.feedback", "a2a.chat"}
	return NewServer(opts)
}

func postForm2(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentsPageListsEveryRoutablePrompt(t *testing.T) {
	h := agentsTestServer(t).HandlerForTest()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	for _, prompt := range []string{"teacher.feedback", "a2a.chat"} {
		if !strings.Contains(rec.Body.String(), prompt) {
			t.Errorf("the page does not offer a provider for %q", prompt)
		}
	}
}

// A pin that reaches the store but that nothing routes is a setting
// which silently does nothing — the worst kind, because the UI confirms
// it. Both bounds are refused.
func TestPinsAreRefusedForUnroutablePromptsAndUnbuiltProviders(t *testing.T) {
	srv := agentsTestServer(t)
	h := srv.HandlerForTest()
	built := srv.opts.AIProviders
	if len(built) == 0 {
		t.Skip("this fixture built no providers, so there is nothing valid to contrast against")
	}

	cases := []struct {
		name, path, provider string
	}{
		{"prompt this process does not route", "/settings/agents/not.a.prompt", built[0]},
		{"provider this process did not build", "/settings/agents/teacher.feedback", "not-a-provider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postForm2(t, h, c.path, url.Values{"provider": {c.provider}})
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303", rec.Code)
			}
			if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
				t.Errorf("redirected to %q with no error to show; the learner would think it saved", loc)
			}
			if got := srv.opts.Settings.PinnedProvider("teacher.feedback"); got != "" {
				t.Errorf("a refused pin was stored anyway: %q", got)
			}
		})
	}
}

func TestPinRoundTripsThroughTheForm(t *testing.T) {
	srv := agentsTestServer(t)
	h := srv.HandlerForTest()
	if len(srv.opts.AIProviders) == 0 {
		t.Skip("no providers built in this fixture")
	}
	want := srv.opts.AIProviders[0]

	if rec := postForm2(t, h, "/settings/agents/teacher.feedback", url.Values{"provider": {want}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, want 303: %s", rec.Code, rec.Body)
	}
	if got := srv.opts.Settings.PinnedProvider("teacher.feedback"); got != want {
		t.Fatalf("pinned provider = %q, want %q", got, want)
	}

	// The empty value is the "設定のまま" option and must clear the pin,
	// not store an empty one.
	if rec := postForm2(t, h, "/settings/agents/teacher.feedback", url.Values{"provider": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear status = %d, want 303", rec.Code)
	}
	if got := srv.opts.Settings.PinnedProvider("teacher.feedback"); got != "" {
		t.Errorf("pin survived being cleared: %q", got)
	}
}
