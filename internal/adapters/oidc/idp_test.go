package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// fakeIDP is a minimal, entirely offline OpenID Connect provider: a
// discovery document, a JWKS, an authorization endpoint and a token
// endpoint, served by httptest over loopback with a key pair generated
// in-process. `make test` must never touch the real Authelia (see the
// task brief's constraints), and every rejection this adapter is
// supposed to perform is only testable by minting a token that is
// wrong in exactly one way — which no real IdP will do on request.
//
// Every knob below exists to make ONE claim or signature wrong while
// leaving the rest of the flow correct, so a test that expects a
// rejection can prove the rejection came from the thing it named.
type fakeIDP struct {
	*httptest.Server
	t *testing.T

	// signKey/signKid are what tokens are actually signed with;
	// jwksKey/jwksKid are what the JWKS publishes. Equal by default —
	// pulling them apart is how the bad-signature and unknown-kid
	// tests are built.
	signKey *rsa.PrivateKey
	signKid string
	jwksKey *rsa.PrivateKey
	jwksKid string

	subject    string
	username   string
	fullName   string
	email      string
	issuerAs   string        // "" ⇒ the real issuer
	audienceAs string        // "" ⇒ the requesting client_id
	lifetime   time.Duration // ID token exp relative to now (may be negative)
	notBefore  time.Duration // nbf relative to now
	issuedAt   time.Duration // iat relative to now
	omitNonce  bool
	nonceAs    string // "" ⇒ the nonce the client actually sent

	// expectChallenge, when non-empty, is what the token endpoint
	// demands the code_verifier hash to instead of the challenge the
	// client presented at /authorize — i.e. a forced PKCE mismatch.
	expectChallenge string

	lastAuthorize url.Values
	lastIDToken   string
	jwksHits      int

	codes map[string]authRequest
}

type authRequest struct {
	nonce     string
	challenge string
	method    string
	clientID  string
	redeemed  bool
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{
		t:        t,
		signKey:  key,
		signKid:  "test-key-1",
		jwksKey:  key,
		jwksKid:  "test-key-1",
		subject:  "8e5b6f28-6a24-4d0e-9d69-2a0a1d0f3a11",
		username: "mikey",
		fullName: "Mikey Austin",
		email:    "mikey@example.invalid",
		lifetime: time.Hour,
		codes:    map[string]authRequest{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("/jwks.json", idp.jwks)
	mux.HandleFunc("/authorize", idp.authorize)
	mux.HandleFunc("/token", idp.token)
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	return idp
}

func (i *fakeIDP) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(i.t, w, map[string]any{
		"issuer":                                i.URL,
		"authorization_endpoint":                i.URL + "/authorize",
		"token_endpoint":                        i.URL + "/token",
		"jwks_uri":                              i.URL + "/jwks.json",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
	})
}

func (i *fakeIDP) jwks(w http.ResponseWriter, _ *http.Request) {
	i.jwksHits++
	pub := i.jwksKey.PublicKey
	writeJSON(i.t, w, map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"alg": "RS256",
		"use": "sig",
		"kid": i.jwksKid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// authorize records what the client asked for (so a test can assert
// PKCE and nonce were actually sent) and bounces straight back to the
// redirect_uri with a code — there is no login UI to drive.
func (i *fakeIDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i.lastAuthorize = q
	code := "code-" + q.Get("state")
	i.codes[code] = authRequest{
		nonce:     q.Get("nonce"),
		challenge: q.Get("code_challenge"),
		method:    q.Get("code_challenge_method"),
		clientID:  q.Get("client_id"),
	}
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := back.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	back.RawQuery = rq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (i *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(i.t, w, "invalid_request")
		return
	}
	// Client authentication: accept either style golang.org/x/oauth2
	// may probe with (basic header or form parameters).
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id != testClientID || secret != testClientSecret {
		oauthError(i.t, w, "invalid_client")
		return
	}
	req, found := i.codes[r.PostFormValue("code")]
	if !found || req.redeemed {
		oauthError(i.t, w, "invalid_grant")
		return
	}
	req.redeemed = true
	i.codes[r.PostFormValue("code")] = req

	want := req.challenge
	if i.expectChallenge != "" {
		want = i.expectChallenge
	}
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if want == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != want {
		oauthError(i.t, w, "invalid_grant")
		return
	}

	nonce := req.nonce
	if i.nonceAs != "" {
		nonce = i.nonceAs
	}
	i.lastIDToken = i.mintIDToken(nonce)
	writeJSON(i.t, w, map[string]any{
		"access_token": "fake-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     i.lastIDToken,
	})
}

func (i *fakeIDP) mintIDToken(nonce string) string {
	now := time.Now()
	iss := i.URL
	if i.issuerAs != "" {
		iss = i.issuerAs
	}
	aud := testClientID
	if i.audienceAs != "" {
		aud = i.audienceAs
	}
	claims := map[string]any{
		"iss":                iss,
		"aud":                []string{aud},
		"sub":                i.subject,
		"exp":                now.Add(i.lifetime).Unix(),
		"iat":                now.Add(i.issuedAt).Unix(),
		"nbf":                now.Add(i.notBefore).Unix(),
		"preferred_username": i.username,
		"name":               i.fullName,
		"email":              i.email,
	}
	if !i.omitNonce {
		claims["nonce"] = nonce
	}
	return signRS256(i.t, i.signKey, i.signKid, claims)
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("fake idp write: %v", err)
	}
}

func oauthError(t *testing.T, w http.ResponseWriter, code string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": code}); err != nil {
		t.Errorf("fake idp write: %v", err)
	}
}
