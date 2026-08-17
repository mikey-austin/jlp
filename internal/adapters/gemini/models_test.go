package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
)

// TestListModelsParsesACapturedResponse pins the lister to
// testdata/models.json — the real body GET /v1beta/models returned for
// this deployment's key, 53 entries of which 37 can serve
// generateContent.
func TestListModelsParsesACapturedResponse(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "models.json"))
	}))
	defer srv.Close()

	l := NewModelLister(testConfig(srv.URL))
	models, err := l.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if gotPath != "/v1beta/models" {
		t.Errorf("path = %q, want /v1beta/models", gotPath)
	}
	if gotKey != testAPIKey {
		t.Errorf("x-goog-api-key = %q, want the configured key", gotKey)
	}
	if len(models) != 37 {
		t.Errorf("got %d models, want the 37 entries this capture reports as generateContent-capable", len(models))
	}
	if !contains(models, "gemini-3-flash-preview") {
		t.Errorf("the model this deployment actually uses is missing from %v", models)
	}
	for _, m := range models {
		if strings.HasPrefix(m, "models/") {
			t.Errorf("model %q still carries the API's resource prefix — APP_AI_GEMINI_MODEL and the generateContent path both want the bare id", m)
			break
		}
	}
	// An embedding-only model is not a chat model: offering one in the
	// /settings dropdown would let an operator pick a value that 400s on
	// every request. Same filtering rule adapters/ollama's lister applies.
	if contains(models, "gemini-embedding-001") {
		t.Error("gemini-embedding-001 was offered — it does not support generateContent")
	}
}

// TestListModelsWithoutAKeyMakesNoCall: dormant-unless-configured. With
// no key there is nothing to authenticate with, so the call is never
// attempted — same contract as adapters/anthropic's lister.
func TestListModelsWithoutAKeyMakesNoCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.APIKey = ""
	_, err := NewModelLister(cfg).ListModels(context.Background())
	if !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("err = %v, want ErrNoAPIKey", err)
	}
	if called {
		t.Error("a request was made without an API key")
	}
}

// TestListModelsErrorsDoNotLeakTheKey: /settings renders this error
// verbatim next to the provider's row.
func TestListModelsErrorsDoNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"API key not valid: ` + testAPIKey + `"}}`))
	}))
	defer srv.Close()

	_, err := NewModelLister(testConfig(srv.URL)).ListModels(context.Background())
	if err == nil {
		t.Fatal("want an error for HTTP 403")
	}
	assertNoKey(t, err.Error())
}

// TestModelListerDefaultsToTheRealAPI: a hand-constructed config.Gemini
// with only an APIKey set must still target the real endpoint rather
// than an empty base URL.
func TestModelListerDefaultsToTheRealAPI(t *testing.T) {
	l := NewModelLister(config.Gemini{APIKey: testAPIKey})
	if l.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", l.baseURL, defaultBaseURL)
	}
}
