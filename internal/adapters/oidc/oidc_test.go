package oidc

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testClientID     = "jlp"
	testClientSecret = "s3cr3t-not-a-real-secret"
	testRedirectURL  = "https://jlp.test/auth/callback"
	testCookieKey    = "0123456789abcdef0123456789abcdef"
)

func newAuth(t *testing.T, idp *fakeIDP, mutate ...func(*Config)) *Authenticator {
	t.Helper()
	cfg := Config{
		IssuerURL:    idp.URL,
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		RedirectURL:  testRedirectURL,
		CookieKey:    []byte(testCookieKey),
		HTTPClient:   idp.Client(),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// flow drives one complete authorization-code round trip against the
// fake IdP and returns the adapter's response to /auth/callback. It
// deliberately re-uses the adapter's own /auth/login handler rather
// than hand-building an authorization URL, so every test exercises the
// state/nonce/PKCE material the adapter actually generated.
type flow struct {
	loginResp    *http.Response
	stateCookie  *http.Cookie
	authorizeURL *url.URL
	callback     *httptest.ResponseRecorder
}

func startLoginFlow(t *testing.T, a *Authenticator, next string) *flow {
	t.Helper()
	target := "/auth/login"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	resp := rec.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/auth/login status = %d, want 302", resp.StatusCode)
	}
	f := &flow{loginResp: resp}
	for _, c := range resp.Cookies() {
		if c.Name == stateCookieName {
			f.stateCookie = c
		}
	}
	if f.stateCookie == nil {
		t.Fatalf("/auth/login set no %q cookie", stateCookieName)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	f.authorizeURL = u
	return f
}

// finish follows the authorization redirect at the IdP and feeds the
// resulting callback (with the state cookie the browser would carry)
// back into the adapter.
func (f *flow) finish(t *testing.T, a *Authenticator, idp *fakeIDP) *httptest.ResponseRecorder {
	t.Helper()
	client := idp.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(f.authorizeURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?"+back.RawQuery, nil)
	if f.stateCookie != nil {
		req.AddCookie(f.stateCookie)
	}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	f.callback = rec
	return rec
}

func login(t *testing.T, a *Authenticator, idp *fakeIDP) *http.Cookie {
	t.Helper()
	rec := startLoginFlow(t, a, "").finish(t, a, idp)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	c := sessionCookieFrom(rec)
	if c == nil {
		t.Fatalf("callback set no %q cookie", sessionCookieName)
	}
	return c
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

func authenticated(t *testing.T, a *Authenticator, c *http.Cookie) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	return r
}

// --- Step 1: the authorization request ------------------------------

func TestLoginSendsPKCEChallengeStateAndNonce(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	f := startLoginFlow(t, a, "/sessions")

	q := f.authorizeURL.Query()
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want code", got)
	}
	if got := q.Get("client_id"); got != testClientID {
		t.Errorf("client_id = %q", got)
	}
	if got := q.Get("redirect_uri"); got != testRedirectURL {
		t.Errorf("redirect_uri = %q", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256 (plain PKCE is not PKCE)", got)
	}
	if q.Get("code_challenge") == "" {
		t.Error("no code_challenge — the authorization code is unbound")
	}
	if q.Get("state") == "" {
		t.Error("no state — the callback is CSRF-open")
	}
	if q.Get("nonce") == "" {
		t.Error("no nonce — the ID token is replayable")
	}
	if got := q.Get("scope"); !strings.Contains(got, "openid") {
		t.Errorf("scope = %q, want it to contain openid", got)
	}
	// The verifier itself must never leave the server: it lives in the
	// signed state cookie, and the whole point of S256 is that the
	// challenge on the wire is not the verifier.
	if strings.Contains(f.authorizeURL.String(), "code_verifier") {
		t.Error("code_verifier appeared in the authorization URL")
	}
}

func TestStateCookieFlags(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp, func(c *Config) { c.CookieSecure = true })
	f := startLoginFlow(t, a, "")
	c := f.stateCookie
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("state cookie flags: HttpOnly=%v Secure=%v SameSite=%v", c.HttpOnly, c.Secure, c.SameSite)
	}
	if c.MaxAge <= 0 || time.Duration(c.MaxAge)*time.Second > loginFlowTTL {
		t.Errorf("state cookie MaxAge = %d, want a short positive TTL", c.MaxAge)
	}
}

// --- Step 2: the happy path -----------------------------------------

func TestCallbackIssuesSessionAndIdentityIsSub(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	f := startLoginFlow(t, a, "/sessions")
	rec := f.finish(t, a, idp)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/sessions" {
		t.Errorf("Location = %q, want the page the login started from", got)
	}
	c := sessionCookieFrom(rec)
	if c == nil {
		t.Fatal("no session cookie")
	}
	id, err := a.Authenticate(authenticated(t, a, c))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if string(id.ID) != idp.subject {
		t.Errorf("identity id = %q, want the sub claim %q", id.ID, idp.subject)
	}
	if id.DisplayName != idp.fullName {
		t.Errorf("display name = %q, want %q", id.DisplayName, idp.fullName)
	}
}

// The identity id is the one thing every row in the database hangs
// off. It must be `sub` even when the token also carries an email and
// a username — an email can be reassigned to another person, a `sub`
// cannot.
func TestIdentityIsNeverEmailOrUsername(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	id, err := a.Authenticate(authenticated(t, a, login(t, a, idp)))
	if err != nil {
		t.Fatal(err)
	}
	if string(id.ID) == idp.email || string(id.ID) == idp.username {
		t.Fatalf("identity id = %q — must be sub, not email/username", id.ID)
	}
}

func TestDisplayNameFallsBackToUsernameThenSub(t *testing.T) {
	idp := newFakeIDP(t)
	idp.fullName = ""
	a := newAuth(t, idp)
	id, err := a.Authenticate(authenticated(t, a, login(t, a, idp)))
	if err != nil {
		t.Fatal(err)
	}
	if id.DisplayName != idp.username {
		t.Errorf("display name = %q, want the preferred_username %q", id.DisplayName, idp.username)
	}

	idp2 := newFakeIDP(t)
	idp2.fullName, idp2.username = "", ""
	a2 := newAuth(t, idp2)
	id2, err := a2.Authenticate(authenticated(t, a2, login(t, a2, idp2)))
	if err != nil {
		t.Fatal(err)
	}
	if id2.DisplayName != idp2.subject {
		t.Errorf("display name = %q, want the sub as last resort", id2.DisplayName)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp, func(c *Config) { c.CookieSecure = true })
	c := login(t, a, idp)
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly — script-readable session")
	}
	if !c.Secure {
		t.Error("session cookie is not Secure")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", c.Path)
	}
}

// The ID token is a bearer credential and the client secret is a
// long-lived one. Neither belongs in a cookie that is written to disk
// on the learner's machine.
func TestSessionCookieCarriesNoTokenOrSecret(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	c := login(t, a, idp)
	if strings.Contains(c.Value, testClientSecret) {
		t.Error("client secret is in the session cookie")
	}
	if strings.Contains(c.Value, idp.lastIDToken) {
		t.Error("the ID token is in the session cookie")
	}
	if strings.Contains(c.Value, "fake-access-token") {
		t.Error("the access token is in the session cookie")
	}
	// And what IS in there is only what the app needs to know who this
	// is — a decoded payload with any other key means something new got
	// stored in a browser without anyone deciding to.
	body, _, _ := strings.Cut(c.Value, ".")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("session cookie payload is not base64: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("session cookie payload is not JSON: %v", err)
	}
	for k := range fields {
		switch k {
		case "sub", "name", "sid", "exp", "seen":
		default:
			t.Errorf("unexpected field %q in the session cookie", k)
		}
	}
}

// --- Step 3: the rejections ------------------------------------------

func TestCallbackRejectsBadSignature(t *testing.T) {
	idp := newFakeIDP(t)
	other := newFakeIDP(t) // only borrowed for its unrelated key pair
	idp.signKey = other.signKey
	// Same kid, different key material: the JWKS lookup succeeds and
	// the signature check is the only thing that can catch this.
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsUnknownKid(t *testing.T) {
	idp := newFakeIDP(t)
	other := newFakeIDP(t)
	idp.signKey, idp.signKid = other.signKey, "not-in-the-jwks"
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsWrongAudience(t *testing.T) {
	idp := newFakeIDP(t)
	idp.audienceAs = "immich"
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsWrongIssuer(t *testing.T) {
	idp := newFakeIDP(t)
	idp.issuerAs = "https://evil.example"
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsExpiredToken(t *testing.T) {
	idp := newFakeIDP(t)
	idp.lifetime = -time.Minute
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsNotYetValidToken(t *testing.T) {
	idp := newFakeIDP(t)
	idp.notBefore = time.Hour
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsTokenIssuedInTheFuture(t *testing.T) {
	idp := newFakeIDP(t)
	idp.issuedAt = time.Hour
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsMissingNonce(t *testing.T) {
	idp := newFakeIDP(t)
	idp.omitNonce = true
	assertLoginRejected(t, idp)
}

// A token minted for somebody else's login attempt — the canonical
// ID-token replay. The nonce we generated and stored is the only thing
// that distinguishes it from ours.
func TestCallbackRejectsReplayedNonce(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	// Capture a nonce from a first, abandoned login…
	first := startLoginFlow(t, a, "")
	stolen := first.authorizeURL.Query().Get("nonce")
	// …and have the IdP mint the SECOND login's token with it.
	idp.nonceAs = stolen
	second := startLoginFlow(t, a, "")
	rec := second.finish(t, a, idp)
	assertRejected(t, rec.Code, rec.Body.String())
	if sessionCookieFrom(rec) != nil {
		t.Fatal("a session was issued for a replayed nonce")
	}
}

func TestCallbackRejectsPKCEVerifierMismatch(t *testing.T) {
	idp := newFakeIDP(t)
	// The token endpoint now demands a verifier hashing to something
	// other than the challenge the client presented — exactly what a
	// stolen authorization code redeemed by an attacker looks like.
	idp.expectChallenge = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	assertLoginRejected(t, idp)
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	f := startLoginFlow(t, a, "")
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=whatever&state=attacker-chosen", nil)
	req.AddCookie(f.stateCookie)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	assertRejected(t, rec.Code, rec.Body.String())
}

func TestCallbackWithoutStateCookieRejected(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	f := startLoginFlow(t, a, "")
	f.stateCookie = nil // the browser never had one — an unsolicited callback
	rec := f.finish(t, a, idp)
	assertRejected(t, rec.Code, rec.Body.String())
}

// The state cookie is single-use: replaying a whole successful
// callback (code, state and cookie together, as an attacker with the
// network trace would) must not mint a second session.
func TestCallbackIsSingleUse(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	f := startLoginFlow(t, a, "")
	if rec := f.finish(t, a, idp); rec.Code != http.StatusFound {
		t.Fatalf("first callback failed: %d %s", rec.Code, rec.Body.String())
	}
	// The adapter must have cleared the state cookie…
	cleared := false
	for _, c := range f.callback.Result().Cookies() {
		if c.Name == stateCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the state cookie was not cleared on success — it stays replayable")
	}
	// …and replaying the exact same request must fail (the code has
	// already been redeemed at the IdP).
	rec := httptest.NewRecorder()
	state := f.authorizeURL.Query().Get("state")
	replay := httptest.NewRequest(http.MethodGet, "/auth/callback?code=code-"+state+"&state="+state, nil)
	replay.AddCookie(f.stateCookie)
	a.Routes().ServeHTTP(rec, replay)
	assertRejected(t, rec.Code, rec.Body.String())
}

func TestNoResponseLeaksTheClientSecret(t *testing.T) {
	idp := newFakeIDP(t)
	idp.expectChallenge = "force-a-token-endpoint-failure"
	a := newAuth(t, idp)
	rec := startLoginFlow(t, a, "").finish(t, a, idp)
	if strings.Contains(rec.Body.String(), testClientSecret) {
		t.Fatal("the client secret reached the error response")
	}
	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Fatal("a token reached the error response")
	}
}

func assertLoginRejected(t *testing.T, idp *fakeIDP) {
	t.Helper()
	a := newAuth(t, idp)
	rec := startLoginFlow(t, a, "").finish(t, a, idp)
	assertRejected(t, rec.Code, rec.Body.String())
	if sessionCookieFrom(rec) != nil {
		t.Fatal("a session cookie was issued despite the rejection")
	}
}

func assertRejected(t *testing.T, code int, body string) {
	t.Helper()
	if code == http.StatusFound || code < 400 {
		t.Fatalf("status = %d, want a 4xx rejection; body=%s", code, body)
	}
}

// --- JWKS caching and rotation ---------------------------------------

func TestJWKSIsCachedAcrossLoginsAndRefreshedOnANewKid(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	login(t, a, idp)
	afterFirst := idp.jwksHits
	login(t, a, idp)
	if idp.jwksHits != afterFirst {
		t.Errorf("JWKS refetched for a known kid (%d → %d hits) — it should be cached", afterFirst, idp.jwksHits)
	}

	rotated := newFakeIDP(t)
	idp.signKey, idp.jwksKey = rotated.signKey, rotated.signKey
	idp.signKid, idp.jwksKid = "test-key-2", "test-key-2"
	login(t, a, idp) // must succeed: an unknown kid forces a refresh
	if idp.jwksHits <= afterFirst {
		t.Error("JWKS was not refetched after key rotation")
	}
}

// --- Sessions --------------------------------------------------------

func TestNoCookieIsUnauthenticated(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	if _, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); err == nil {
		t.Fatal("expected rejection with no cookie")
	}
}

func TestTamperedSessionCookieRejected(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	c := login(t, a, idp)
	for _, tampered := range []string{
		c.Value + "x",
		strings.Replace(c.Value, ".", ".x", 1),
		"garbage",
		"",
	} {
		bad := &http.Cookie{Name: sessionCookieName, Value: tampered}
		if _, err := a.Authenticate(authenticated(t, a, bad)); err == nil {
			t.Errorf("tampered cookie %q was accepted", tampered)
		}
	}
}

// A cookie signed with a different key must not authenticate — this is
// what makes the cookie key, not the cookie's contents, the thing that
// has to stay secret.
func TestSessionCookieFromAnotherKeyRejected(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	c := login(t, a, idp)
	b := newAuth(t, idp, func(cfg *Config) { cfg.CookieKey = []byte("ffffffffffffffffffffffffffffffff") })
	if _, err := b.Authenticate(authenticated(t, b, c)); err == nil {
		t.Fatal("a cookie signed with another key was accepted")
	}
}

func TestSessionExpiresAtItsAbsoluteLifetime(t *testing.T) {
	idp := newFakeIDP(t)
	now := time.Now()
	a := newAuth(t, idp, func(c *Config) {
		c.SessionTTL = time.Hour
		c.IdleTimeout = time.Hour
		c.Now = func() time.Time { return now }
	})
	c := login(t, a, idp)
	now = now.Add(time.Hour + time.Second)
	if _, err := a.Authenticate(authenticated(t, a, c)); err == nil {
		t.Fatal("session outlived its absolute lifetime")
	}
}

func TestSessionExpiresAfterIdleTimeout(t *testing.T) {
	idp := newFakeIDP(t)
	now := time.Now()
	a := newAuth(t, idp, func(c *Config) {
		c.SessionTTL = 24 * time.Hour
		c.IdleTimeout = 30 * time.Minute
		c.Now = func() time.Time { return now }
	})
	c := login(t, a, idp)
	now = now.Add(31 * time.Minute)
	if _, err := a.Authenticate(authenticated(t, a, c)); err == nil {
		t.Fatal("session survived the idle timeout")
	}
}

func TestKeepAliveExtendsTheIdleDeadline(t *testing.T) {
	idp := newFakeIDP(t)
	now := time.Now()
	a := newAuth(t, idp, func(c *Config) {
		c.SessionTTL = 24 * time.Hour
		c.IdleTimeout = 30 * time.Minute
		c.Now = func() time.Time { return now }
	})
	c := login(t, a, idp)

	now = now.Add(20 * time.Minute)
	req := authenticated(t, a, c)
	if _, err := a.Authenticate(req); err != nil {
		t.Fatalf("session should still be live: %v", err)
	}
	rec := httptest.NewRecorder()
	a.KeepAlive(rec, req)
	refreshed := sessionCookieFrom(rec)
	if refreshed == nil {
		t.Fatal("KeepAlive did not re-issue the session cookie")
	}

	now = now.Add(20 * time.Minute) // 40 min after login, 20 after activity
	if _, err := a.Authenticate(authenticated(t, a, refreshed)); err != nil {
		t.Fatalf("refreshed session expired anyway: %v", err)
	}
	if _, err := a.Authenticate(authenticated(t, a, c)); err == nil {
		t.Fatal("the pre-refresh cookie should have gone idle")
	}
}

// KeepAlive runs on every authenticated request, before the handler.
// Setting a cookie on all of them would be wasteful and would defeat
// caching, so it must stay quiet until the session is worth touching.
func TestKeepAliveIsQuietWhenNothingHasChanged(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	c := login(t, a, idp)
	rec := httptest.NewRecorder()
	a.KeepAlive(rec, authenticated(t, a, c))
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("KeepAlive wrote Set-Cookie on a fresh session: %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("KeepAlive wrote a body: %q", rec.Body.String())
	}
}

func TestKeepAliveOnAnUnauthenticatedRequestIsANoOp(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	rec := httptest.NewRecorder()
	a.KeepAlive(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Set-Cookie") != "" || rec.Body.Len() != 0 {
		t.Error("KeepAlive wrote to the response for an unauthenticated request")
	}
}

// --- Logout ----------------------------------------------------------

func TestLogoutClearsTheSessionAndRedirects(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp, func(c *Config) { c.LogoutURL = "https://auth.test/logout" })
	c := login(t, a, idp)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://auth.test/logout" {
		t.Errorf("logout Location = %q", got)
	}
	cleared := sessionCookieFrom(rec)
	if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("logout did not clear the session cookie: %+v", cleared)
	}
	// The cookie the browser was holding must no longer authenticate
	// anything even if it is replayed — the cleared cookie is only a
	// hint to a cooperating browser.
	if _, err := a.Authenticate(authenticated(t, a, c)); err == nil {
		t.Fatal("the pre-logout cookie still authenticates")
	}
}

func TestCrossSiteLogoutRejected(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp, func(c *Config) { c.LogoutURL = "https://auth.test/logout" })
	c := login(t, a, idp)
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if _, err := a.Authenticate(authenticated(t, a, c)); err != nil {
		t.Fatal("a cross-site request signed the learner out anyway")
	}
}

func TestLogoutWithoutASessionStillRedirects(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp, func(c *Config) { c.LogoutURL = "https://auth.test/logout" })
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/logout", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want 302", rec.Code)
	}
}

// --- StartLogin (the auth.Interactive half) --------------------------

func TestStartLoginRedirectsBrowserToLoginPreservingTheTarget(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	rec := httptest.NewRecorder()
	a.StartLogin(rec, httptest.NewRequest(http.MethodGet, "/sessions/42?tab=notes", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Path != loginPath {
		t.Errorf("Location path = %q, want %q", loc.Path, loginPath)
	}
	if got := loc.Query().Get("next"); got != "/sessions/42?tab=notes" {
		t.Errorf("next = %q", got)
	}
}

// htmx swaps a fragment into the current page; a 302 to the IdP would
// be followed by XHR and land as an opaque CORS failure, leaving the
// learner staring at a page that silently stopped working.
func TestStartLoginUsesHXRedirectForHtmxRequests(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	req := httptest.NewRequest(http.MethodPost, "/sessions/42/feedback", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "https://jlp.test/sessions/42")
	rec := httptest.NewRecorder()
	a.StartLogin(rec, req)

	loc, err := url.Parse(rec.Header().Get("HX-Redirect"))
	if err != nil || loc.Path != loginPath {
		t.Fatalf("HX-Redirect = %q (err=%v)", rec.Header().Get("HX-Redirect"), err)
	}
	if got := loc.Query().Get("next"); got != "/sessions/42" {
		t.Errorf("next = %q, want the page the learner was on", got)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("an htmx request also got a Location redirect")
	}
}

// An open redirector inside the login flow would let a phishing page
// bounce a victim through JLP's own domain.
func TestNextIsRestrictedToLocalPaths(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuth(t, idp)
	for _, next := range []string{
		"https://evil.example/steal",
		"//evil.example/steal",
		"/\\evil.example",
		"http://evil.example",
		"javascript:alert(1)",
	} {
		rec := startLoginFlow(t, a, next).finish(t, a, idp)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback failed for next=%q: %d", next, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/" {
			t.Errorf("next=%q redirected to %q, want /", next, got)
		}
	}
}

// --- Configuration ---------------------------------------------------

func TestNewRejectsIncompleteConfiguration(t *testing.T) {
	base := func() Config {
		return Config{
			IssuerURL:    "https://auth.test",
			ClientID:     testClientID,
			ClientSecret: testClientSecret,
			RedirectURL:  testRedirectURL,
			CookieKey:    []byte(testCookieKey),
		}
	}
	cases := map[string]func(*Config){
		"no issuer":        func(c *Config) { c.IssuerURL = "" },
		"no client id":     func(c *Config) { c.ClientID = "" },
		"no client secret": func(c *Config) { c.ClientSecret = "" },
		"no redirect uri":  func(c *Config) { c.RedirectURL = "" },
		"no cookie key":    func(c *Config) { c.CookieKey = nil },
		"short cookie key": func(c *Config) { c.CookieKey = []byte("too-short") },
		"http issuer":      func(c *Config) { c.IssuerURL = "http://auth.test" },
	}
	for name, break_ := range cases {
		cfg := base()
		break_(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		} else if strings.Contains(err.Error(), testClientSecret) {
			t.Errorf("%s: the error leaks the client secret", name)
		}
	}
}

// The one exception to the https-issuer rule: a loopback issuer, which
// is what every test in this file uses and the only way an offline
// test suite can exist at all.
func TestLoopbackIssuerIsAllowedForTesting(t *testing.T) {
	idp := newFakeIDP(t)
	if _, err := New(Config{
		IssuerURL: idp.URL, ClientID: testClientID, ClientSecret: testClientSecret,
		RedirectURL: testRedirectURL, CookieKey: []byte(testCookieKey),
	}); err != nil {
		t.Fatalf("loopback issuer rejected: %v", err)
	}
}
