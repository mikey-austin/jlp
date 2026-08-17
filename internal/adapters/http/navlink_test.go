package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The A2A chat is a separately deployed service on its own host. Linking
// to it unconditionally would put a dead nav item on every dev machine
// and every deployment that doesn't run it — and a nav item that 404s is
// worse than no nav item, because it looks like the app is broken.
//
// Both cases render /settings/tokens rather than /sessions, and both
// assert the page actually rendered. The first draft of this test used
// /sessions, which 500s under testOptions (no Sessions service): the
// "link is absent" case passed against an EMPTY BODY and proved
// nothing.
func TestChatLinkAppearsOnlyWhenTheChatIsDeployed(t *testing.T) {
	t.Run("absent by default", func(t *testing.T) {
		h := NewServer(testOptions()).HandlerForTest()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/tokens", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("page did not render (%d); an absence check against a blank page proves nothing", rec.Code)
		}
		// Match the LINK, not the word: the tokens page's own prose
		// mentions "A2Aチャット", so a bare word check failed against
		// entirely correct markup.
		if strings.Contains(rec.Body.String(), `>チャット</a>`) {
			t.Error("the nav links to the A2A chat with no A2AChatURL configured, so the link goes nowhere")
		}
	})

	t.Run("present when configured", func(t *testing.T) {
		opts := testOptions()
		opts.A2AChatURL = "https://a2a.example.test"
		h := NewServer(opts).HandlerForTest()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/tokens", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("page did not render (%d)", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "https://a2a.example.test") {
			t.Error("A2AChatURL is set but the nav does not link to it")
		}
		// It leaves the app, so it must not hand the target a window
		// handle back into this one.
		if !strings.Contains(body, `rel="noopener"`) {
			t.Error("the outbound chat link has no rel=noopener")
		}
	})
}
