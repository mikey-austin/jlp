// Package oidc authenticates learners against an OpenID Connect
// provider (Authelia, in this deployment) using the authorization code
// flow with PKCE, and keeps them signed in with its own signed session
// cookie.
//
// It is the third implementation of the auth.Authenticator port, and
// it exists to replace what internal/adapters/authelia does with
// something that does not depend on where a packet came from. That
// adapter reads plain, unsigned Remote-User headers and believes them
// when the TCP peer's address is in a list: the entire security
// boundary is a source-IP match, which fails open the moment the proxy
// moves, the backend port becomes reachable another way, or anything
// else runs on the proxy host. Nothing about those headers is
// verifiable — Authelia's forward-auth deliberately emits plain
// strings, not a signed assertion.
//
// Here the trust is cryptographic instead. The learner's browser goes
// to the provider, comes back with an authorization code bound to a
// PKCE verifier only this process knows, and the ID token that code
// buys is verified against the provider's published JWKS: signature,
// issuer, audience, expiry, not-before, issued-at, and the nonce this
// process generated for this one login. A request from the "right"
// address proves nothing here, and one from the "wrong" address is not
// penalised.
//
// Three deliberate choices worth knowing before changing anything:
//
//   - The identity id is the `sub` claim and nothing else. Every row in
//     JLP's database hangs off the identity id; an email or a username
//     can be reassigned to a different human, and `sub` cannot. See
//     identityFrom.
//   - The session cookie is signed, not encrypted, and carries no
//     token. It holds the sub, a display name and three timestamps —
//     nothing whose disclosure matters, so integrity is the only
//     property needed. The ID token is verified once, at callback, and
//     then dropped.
//   - Discovery is lazy. The provider document is fetched on first use,
//     not at construction, so JLP boots (and already-signed-in
//     learners keep working) when the IdP is briefly down — a hard
//     boot-time dependency between two containers that start together
//     is a restart loop waiting to happen.
package oidc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

const (
	// basePath and the three routes under it. They are absolute (rather
	// than relative to a mount point) because internal/adapters/http
	// attaches Routes() with a plain "/auth/*" pattern, which leaves
	// r.URL.Path untouched — see NewServer's route table.
	basePath     = "/auth"
	loginPath    = basePath + "/login"
	callbackPath = basePath + "/callback"
	// LogoutPath is exported because the page layout needs to render a
	// form that posts to it (see internal/adapters/http Options.LogoutPath).
	LogoutPath = basePath + "/logout"

	sessionCookieName = "jlp_session"
	// The login cookie holds the in-flight state/nonce/PKCE verifier.
	// Separate from the session cookie because it has a completely
	// different lifetime (one redirect round trip) and is deleted the
	// moment the callback consumes it.
	stateCookieName = "jlp_login"

	// loginFlowTTL bounds how long a started login may take to come
	// back. Long enough to type a password and a TOTP code, short
	// enough that an abandoned flow's state is not sitting in a browser
	// for a day.
	loginFlowTTL = 10 * time.Minute

	defaultSessionTTL  = 24 * time.Hour
	defaultIdleTimeout = 8 * time.Hour

	// clockSkewLeeway is what iat/nbf are allowed to be ahead of us by.
	// Five minutes matches go-oidc's own nbf leeway (see verify.go), so
	// the two halves of the time validation agree.
	clockSkewLeeway = 5 * time.Minute

	// minCookieKeyLen is the shortest HMAC-SHA256 key accepted. 32
	// bytes is the hash's own block-relevant size; anything shorter is
	// a config mistake, not a tradeoff.
	minCookieKeyLen = 32
)

// Config is everything the adapter needs. Nothing here is read from
// the environment directly — internal/config owns that, this package
// owns the flow (Rule 2: adapters take their configuration).
type Config struct {
	// IssuerURL is the provider's issuer identifier, e.g.
	// https://auth.lan.jackiemclean.net. Discovery appends
	// /.well-known/openid-configuration to it, and the ID token's `iss`
	// must equal it exactly.
	IssuerURL string
	// ClientID is also the audience every accepted ID token must carry.
	ClientID string
	// ClientSecret authenticates JLP to the token endpoint. It must
	// never appear in a log line, an error, or a response body — see
	// safeOAuthError.
	ClientSecret string
	// RedirectURL must match a redirect_uri registered with the
	// provider, byte for byte.
	RedirectURL string
	// Scopes defaults to openid+profile+email+groups. `openid` is added
	// if a caller supplies scopes without it — a request without it is
	// not an OIDC request at all and would come back with no ID token.
	Scopes []string
	// CookieKey signs the session and login cookies (HMAC-SHA256).
	// Changing it invalidates every live session, which is the intended
	// emergency lever.
	CookieKey []byte
	// CookieSecure marks both cookies Secure. Configurable only so the
	// offline tests can drive the flow over loopback http; every real
	// deployment sets it.
	CookieSecure bool
	// SessionTTL is the absolute lifetime of a session: after it, the
	// learner logs in again no matter how active they were.
	SessionTTL time.Duration
	// IdleTimeout ends a session that has gone quiet. KeepAlive
	// extends it while the learner is using the app.
	IdleTimeout time.Duration
	// LogoutURL is where the browser is sent after the local session is
	// cleared. When empty, the provider's advertised
	// end_session_endpoint is used if it has one, and failing that the
	// learner simply lands back on "/" — see logoutTarget for why this
	// deployment needs the override.
	LogoutURL string
	// HTTPClient talks to the provider (discovery, JWKS, token
	// endpoint). Optional; http.DefaultClient otherwise.
	HTTPClient *http.Client
	// Now is the clock, injectable so the session-lifetime tests do not
	// have to sleep. It never affects ID token validation, which uses
	// go-oidc's own clock.
	Now func() time.Time
}

// Authenticator implements auth.Authenticator and auth.Interactive.
type Authenticator struct {
	cfg    Config
	routes http.Handler

	// discovery, lazily resolved and then cached forever — see the
	// package doc comment for why not at construction.
	discoverMu sync.Mutex
	oauth      *oauth2.Config
	verifier   *gooidc.IDTokenVerifier
	endSession string

	// revoked holds session ids ended by an explicit logout, each
	// mapped to the unix time its cookie would have expired on its own
	// (so the map cannot grow without bound). Without it, logout would
	// be nothing but a Set-Cookie the browser is free to ignore: a
	// stateless signed cookie is valid until it expires, so anyone
	// holding a copy could keep using it. Deliberately in memory only:
	// a process restart forgets the revocations, which is the one gap
	// here — closing it would mean a sessions table and a database
	// round trip on every request, for a single-learner deployment
	// where the realistic threat is a shared browser, not a stolen
	// cookie surviving a redeploy.
	revokedMu sync.Mutex
	revoked   map[string]int64
}

var _ auth.Interactive = (*Authenticator)(nil)

// New validates cfg and builds the adapter. It performs no network I/O
// — a wrong issuer is not diagnosed here but on the first login.
func New(cfg Config) (*Authenticator, error) {
	if cfg.IssuerURL == "" {
		return nil, errors.New("oidc: issuer URL is required")
	}
	iss, err := url.Parse(cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: issuer URL %q is not a URL: %w", cfg.IssuerURL, err)
	}
	if iss.Scheme != "https" && !isLoopback(iss.Hostname()) {
		return nil, fmt.Errorf("oidc: issuer %q must be https (loopback is allowed for tests only) — the ID token and the client secret both cross this connection", cfg.IssuerURL)
	}
	if cfg.ClientID == "" {
		return nil, errors.New("oidc: client id is required")
	}
	if cfg.ClientSecret == "" {
		return nil, errors.New("oidc: client secret is required")
	}
	if cfg.RedirectURL == "" {
		return nil, errors.New("oidc: redirect URL is required")
	}
	if len(cfg.CookieKey) < minCookieKeyLen {
		return nil, fmt.Errorf("oidc: cookie key must be at least %d bytes, got %d", minCookieKeyLen, len(cfg.CookieKey))
	}
	cfg.IssuerURL = strings.TrimSuffix(cfg.IssuerURL, "/")
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{gooidc.ScopeOpenID, "profile", "email", "groups"}
	} else if !contains(cfg.Scopes, gooidc.ScopeOpenID) {
		cfg.Scopes = append([]string{gooidc.ScopeOpenID}, cfg.Scopes...)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	if cfg.IdleTimeout <= 0 || cfg.IdleTimeout > cfg.SessionTTL {
		cfg.IdleTimeout = min(defaultIdleTimeout, cfg.SessionTTL)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &Authenticator{cfg: cfg, revoked: map[string]int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc(loginPath, a.handleLogin)
	mux.HandleFunc(callbackPath, a.handleCallback)
	mux.HandleFunc(LogoutPath, a.handleLogout)
	a.routes = mux
	return a, nil
}

// Routes serves /auth/login, /auth/callback and /auth/logout. It must
// be mounted OUTSIDE the authenticated route group — these are the
// three requests a learner makes precisely because they have no
// session yet.
func (a *Authenticator) Routes() http.Handler { return a.routes }

// Authenticate resolves the session cookie. It never talks to the
// provider: that happened once, at callback, and the whole point of
// the cookie is that every subsequent request is a local HMAC check.
func (a *Authenticator) Authenticate(r *http.Request) (learner.Identity, error) {
	s, err := a.readSession(r)
	if err != nil {
		return learner.Identity{}, err
	}
	return learner.Identity{ID: learner.IdentityID(s.Sub), DisplayName: s.Name}, nil
}

// StartLogin sends the browser to /auth/login, remembering where it
// was trying to go. It deliberately does NOT redirect straight to the
// provider: /auth/login is the one place that mints state, nonce and
// PKCE material, and having two of them would be two places to get it
// wrong.
func (a *Authenticator) StartLogin(w http.ResponseWriter, r *http.Request) {
	target := loginPath
	if next := safeNext(loginTargetOf(r)); next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	// htmx drives most of this app. It issues XHR and swaps a fragment
	// into the live page, so a 302 here would be followed by the XHR
	// itself and die as an opaque cross-origin failure at the IdP,
	// leaving the learner on a page that quietly stopped responding.
	// HX-Redirect is htmx's own instruction to navigate the whole tab.
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// KeepAlive slides the idle deadline forward. It re-issues the cookie
// only once the session is at least halfway to going idle: this runs
// before every authenticated handler, and a Set-Cookie on every
// response would be pure overhead (and would make every response
// uncacheable) for no extra safety.
func (a *Authenticator) KeepAlive(w http.ResponseWriter, r *http.Request) {
	s, err := a.readSession(r)
	if err != nil {
		return // not our session to extend; RequireIdentity already dealt with it
	}
	now := a.cfg.Now()
	if now.Sub(time.Unix(s.Seen, 0)) < a.cfg.IdleTimeout/2 {
		return
	}
	s.Seen = now.Unix()
	a.setSessionCookie(w, s)
}

// --- the flow --------------------------------------------------------

func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	oauthCfg, _, err := a.discover(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, "the sign-in service is unavailable", "discovery", err)
		return
	}
	state, err := randomToken()
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "could not start sign-in", "state", err)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "could not start sign-in", "nonce", err)
		return
	}
	verifier := oauth2.GenerateVerifier()
	flow := loginFlow{
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
		Next:     safeNext(r.URL.Query().Get("next")),
		Exp:      a.cfg.Now().Add(loginFlowTTL).Unix(),
	}
	if !a.setCookie(w, stateCookieName, flow, int(loginFlowTTL.Seconds())) {
		a.fail(w, r, http.StatusInternalServerError, "could not start sign-in", "state cookie", nil)
		return
	}
	http.Redirect(w, r, oauthCfg.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), http.StatusFound)
}

func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	var flow loginFlow
	if !a.readCookie(r, stateCookieName, &flow) {
		a.fail(w, r, http.StatusBadRequest, "sign-in expired — please try again", "no login cookie", nil)
		return
	}
	// Single use, whatever happens next: the cookie is cleared before
	// the code is redeemed, so neither a success nor a failure leaves
	// replayable state in the browser.
	a.clearCookie(w, stateCookieName)
	if a.cfg.Now().Unix() > flow.Exp {
		a.fail(w, r, http.StatusBadRequest, "sign-in expired — please try again", "login flow expired", nil)
		return
	}
	if subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
		a.fail(w, r, http.StatusBadRequest, "sign-in could not be verified — please try again", "state mismatch", nil)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		// The provider's own refusal (access_denied, consent_required…).
		a.fail(w, r, http.StatusUnauthorized, "sign-in was refused", "provider returned "+sanitiseForLog(e), nil)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.fail(w, r, http.StatusBadRequest, "sign-in could not be completed — please try again", "no authorization code", nil)
		return
	}

	oauthCfg, verifier, err := a.discover(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusBadGateway, "the sign-in service is unavailable", "discovery", err)
		return
	}
	ctx := a.clientContext(r.Context())
	// The PKCE verifier goes up with the code. Without it the token
	// endpoint rejects the exchange, which is what makes a stolen
	// authorization code worthless to anyone but this process.
	token, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		a.fail(w, r, http.StatusUnauthorized, "sign-in could not be completed — please try again", "code exchange: "+safeOAuthError(err), nil)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		a.fail(w, r, http.StatusUnauthorized, "sign-in could not be completed — please try again", "no id_token in the token response", nil)
		return
	}
	// Signature (against the cached JWKS, refetched if this kid is new),
	// issuer, audience, expiry and nbf — all of it inside go-oidc.
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		a.fail(w, r, http.StatusUnauthorized, "sign-in could not be verified", "id token: "+sanitiseForLog(err.Error()), nil)
		return
	}
	var claims idTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		a.fail(w, r, http.StatusUnauthorized, "sign-in could not be verified", "id token claims", nil)
		return
	}
	// The nonce binds this token to the login THIS browser started.
	// Without it, a token minted for another login (or another relying
	// party's session) could be replayed into ours.
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(flow.Nonce)) != 1 {
		a.fail(w, r, http.StatusUnauthorized, "sign-in could not be verified", "nonce mismatch", nil)
		return
	}
	if err := a.checkIssuedTimes(claims); err != nil {
		a.fail(w, r, http.StatusUnauthorized, "sign-in could not be verified", err.Error(), nil)
		return
	}
	if idToken.Subject == "" {
		a.fail(w, r, http.StatusUnauthorized, "sign-in could not be verified", "no sub claim", nil)
		return
	}

	sid, err := randomToken()
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "could not complete sign-in", "session id", err)
		return
	}
	now := a.cfg.Now()
	s := session{
		Sub:  idToken.Subject,
		Name: displayNameFrom(idToken.Subject, claims),
		SID:  sid,
		Exp:  now.Add(a.cfg.SessionTTL).Unix(),
		Seen: now.Unix(),
	}
	if !a.setSessionCookie(w, s) {
		a.fail(w, r, http.StatusInternalServerError, "could not complete sign-in", "session cookie", nil)
		return
	}
	// Deliberately no identifying detail: "who signed in" is a fact
	// about a person, and the sub is the key to their whole dataset.
	slog.Info("oidc: sign-in completed")
	next := flow.Next
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	// /auth/* is mounted outside the group internal/adapters/http's
	// CSRFProtect guards (it has to be — none of these requests have a
	// session), so this one route does its own check. Forced logout is
	// a nuisance rather than a breach, but "some page I visited signed
	// me out mid-sentence" is still a bug, and Sec-Fetch-Site is
	// browser-set and unforgeable by a page.
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		a.fail(w, r, http.StatusForbidden, "cross-origin request rejected", "cross-site logout", nil)
		return
	}
	if s, err := a.readSession(r); err == nil {
		a.revoke(s)
	}
	a.clearCookie(w, sessionCookieName)
	http.Redirect(w, r, a.logoutTarget(r.Context()), http.StatusFound)
}

// logoutTarget is where the browser goes after the local session is
// gone. Preference order: the configured URL, the provider's
// advertised end_session_endpoint, then "/".
//
// The middle option is the standard one (RP-initiated logout) and the
// one this adapter would use by default — but the Authelia this
// deployment runs does not advertise end_session_endpoint at all (its
// discovery document has no such key), so ending the SSO session there
// means sending the browser to Authelia's own logout page instead.
// That is what LogoutURL is for, and why logging out of JLP logs the
// learner out of the identity provider as a whole: with no RP-initiated
// logout available, the alternative is a "logout" that leaves them
// able to sign straight back in with one click, which is not what the
// button says it does.
func (a *Authenticator) logoutTarget(ctx context.Context) string {
	if a.cfg.LogoutURL != "" {
		return a.cfg.LogoutURL
	}
	if _, _, err := a.discover(ctx); err != nil {
		// Signing out must not fail because the provider is down: the
		// local session is already gone by the time we get here.
		return "/"
	}
	a.discoverMu.Lock()
	defer a.discoverMu.Unlock()
	if a.endSession != "" {
		return a.endSession
	}
	return "/"
}

// --- provider discovery ----------------------------------------------

func (a *Authenticator) discover(ctx context.Context) (*oauth2.Config, *gooidc.IDTokenVerifier, error) {
	a.discoverMu.Lock()
	defer a.discoverMu.Unlock()
	if a.oauth != nil {
		return a.oauth, a.verifier, nil
	}
	provider, err := gooidc.NewProvider(a.clientContext(ctx), a.cfg.IssuerURL)
	if err != nil {
		// %w keeps the transport error; it contains a URL and a status,
		// never a credential (the discovery request carries none).
		return nil, nil, fmt.Errorf("oidc: provider discovery at %s: %w", a.cfg.IssuerURL, err)
	}
	var extra struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&extra); err == nil {
		a.endSession = extra.EndSessionEndpoint
	}
	a.oauth = &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  a.cfg.RedirectURL,
		Scopes:       a.cfg.Scopes,
	}
	a.verifier = provider.Verifier(&gooidc.Config{ClientID: a.cfg.ClientID})
	return a.oauth, a.verifier, nil
}

func (a *Authenticator) clientContext(ctx context.Context) context.Context {
	if a.cfg.HTTPClient == nil {
		return ctx
	}
	return gooidc.ClientContext(ctx, a.cfg.HTTPClient)
}

// --- claims -----------------------------------------------------------

type idTokenClaims struct {
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	Email             string `json:"email"`
	IssuedAt          int64  `json:"iat"`
	NotBefore         int64  `json:"nbf"`
}

// displayNameFrom picks what to show in the header. Note what it does
// NOT do: it never influences the identity id. `name` first because
// that is the human's own rendering of their name,
// `preferred_username` next, and the sub last so the header is never
// blank.
func displayNameFrom(sub string, c idTokenClaims) string {
	if c.Name != "" {
		return c.Name
	}
	if c.PreferredUsername != "" {
		return c.PreferredUsername
	}
	return sub
}

// checkIssuedTimes covers the two time claims go-oidc's verifier does
// not: it enforces exp itself and nbf with a 5-minute leeway, but
// accepts any iat. A token issued in the future is either a badly
// skewed clock or a forgery attempt, and neither should authenticate.
func (a *Authenticator) checkIssuedTimes(c idTokenClaims) error {
	now := time.Now()
	if c.IssuedAt != 0 && time.Unix(c.IssuedAt, 0).After(now.Add(clockSkewLeeway)) {
		return errors.New("id token issued in the future")
	}
	if c.NotBefore != 0 && time.Unix(c.NotBefore, 0).After(now.Add(clockSkewLeeway)) {
		return errors.New("id token is not yet valid")
	}
	return nil
}

// --- sessions ---------------------------------------------------------

type session struct {
	Sub  string `json:"sub"`
	Name string `json:"name,omitempty"`
	// SID identifies this session so logout can revoke it — see
	// Authenticator.revoked.
	SID  string `json:"sid"`
	Exp  int64  `json:"exp"`
	Seen int64  `json:"seen"`
}

type loginFlow struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next,omitempty"`
	Exp      int64  `json:"exp"`
}

func (a *Authenticator) readSession(r *http.Request) (session, error) {
	var s session
	if !a.readCookie(r, sessionCookieName, &s) {
		return session{}, auth.ErrUnauthenticated
	}
	now := a.cfg.Now()
	if s.Sub == "" || now.Unix() > s.Exp {
		return session{}, auth.ErrUnauthenticated
	}
	if now.Sub(time.Unix(s.Seen, 0)) > a.cfg.IdleTimeout {
		return session{}, auth.ErrUnauthenticated
	}
	if a.isRevoked(s.SID) {
		return session{}, auth.ErrUnauthenticated
	}
	return s, nil
}

func (a *Authenticator) setSessionCookie(w http.ResponseWriter, s session) bool {
	maxAge := int(time.Unix(s.Exp, 0).Sub(a.cfg.Now()).Seconds())
	if maxAge <= 0 {
		return false
	}
	return a.setCookie(w, sessionCookieName, s, maxAge)
}

func (a *Authenticator) revoke(s session) {
	a.revokedMu.Lock()
	defer a.revokedMu.Unlock()
	now := a.cfg.Now().Unix()
	for sid, exp := range a.revoked {
		if exp < now {
			delete(a.revoked, sid) // it could not authenticate anyway
		}
	}
	a.revoked[s.SID] = s.Exp
}

func (a *Authenticator) isRevoked(sid string) bool {
	if sid == "" {
		return false
	}
	a.revokedMu.Lock()
	defer a.revokedMu.Unlock()
	_, ok := a.revoked[sid]
	return ok
}

// --- signed cookies ----------------------------------------------------

// setCookie writes v as base64(json).base64(HMAC-SHA256(json)). Signed
// rather than encrypted on purpose: nothing in either payload is
// secret (a sub, a display name, timestamps, and a PKCE verifier that
// is worthless without the matching authorization code and client
// secret), so integrity is the only property that has to hold.
func (a *Authenticator) setCookie(w http.ResponseWriter, name string, v any, maxAge int) bool {
	payload, err := json.Marshal(v)
	if err != nil {
		slog.Error("oidc: encode cookie", "cookie", name, "err", err)
		return false
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    body + "." + a.sign(body),
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	return true
}

func (a *Authenticator) readCookie(r *http.Request, name string, v any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	body, mac, found := strings.Cut(c.Value, ".")
	if !found {
		return false
	}
	if !hmac.Equal([]byte(mac), []byte(a.sign(body))) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	return json.Unmarshal(payload, v) == nil
}

func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *Authenticator) sign(body string) string {
	m := hmac.New(sha256.New, a.cfg.CookieKey)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// --- helpers -----------------------------------------------------------

// fail writes a deliberately vague message to the learner and the
// specific one to the log. The split matters here more than in most
// handlers: these errors are produced while holding a token, and a
// helpful "the audience was X, expected Y" in an HTTP body is a gift
// to whoever is probing the endpoint. cause is already-safe text; err
// is only ever a local/transport error, never one carrying a token.
func (a *Authenticator) fail(w http.ResponseWriter, r *http.Request, status int, message, cause string, err error) {
	if err != nil {
		slog.Warn("oidc: "+cause, "path", r.URL.Path, "err", err)
	} else {
		slog.Warn("oidc: sign-in rejected", "path", r.URL.Path, "reason", cause)
	}
	http.Error(w, message, status)
}

// safeOAuthError renders a token-endpoint failure without its body.
// oauth2.RetrieveError.Error() embeds the raw HTTP response, which is
// exactly the kind of thing that ends up in a log with a token in it.
func safeOAuthError(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		code := re.ErrorCode
		if code == "" {
			code = "no error code"
		}
		return fmt.Sprintf("token endpoint returned %d (%s)", re.Response.StatusCode, sanitiseForLog(code))
	}
	// Anything else is a transport error: a dial failure, a TLS
	// problem, a context cancellation. None of those carry the request
	// body, and the request is the only place the secret appears.
	return sanitiseForLog(err.Error())
}

// sanitiseForLog keeps provider-controlled strings from forging log
// structure (they are already going into a quoted slog value, but a
// newline in a message is the classic way to fake a second log line)
// and bounds their length.
func sanitiseForLog(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// safeNext restricts post-login redirects to paths on this site. An
// unchecked `next` is an open redirector, and one on the login route
// of all places: a phishing page could bounce a victim through JLP's
// own domain, arriving at the attacker's page with JLP's URL in the
// referrer and the victim's trust already spent.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") {
		return ""
	}
	// "//host" and "/\host" are both protocol-relative URLs to another
	// origin that still start with a single "/".
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return ""
	}
	// This ends up in a Location (or HX-Redirect) header. Go's own
	// header writer would reject a newline here, turning a crafted
	// `next` into a 500 rather than a smuggled header — but failing on
	// the input is better than failing on the output, and a control
	// character has no business in a path either way.
	if strings.ContainsFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" {
		return ""
	}
	return next
}

// loginTargetOf is where the learner should land once signed in: the
// URL they asked for, or — for an htmx request, whose own URL is a
// fragment endpoint like /sessions/42/feedback — the page they were
// looking at when it fired.
func loginTargetOf(r *http.Request) string {
	if cur := r.Header.Get("HX-Current-URL"); cur != "" {
		if u, err := url.Parse(cur); err == nil && u.Path != "" {
			return u.RequestURI()
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return ""
	}
	return r.URL.RequestURI()
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
