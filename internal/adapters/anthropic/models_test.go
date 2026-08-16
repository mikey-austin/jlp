package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
)

const cannedModelsListResponse = `{"data":[
 {"id":"claude-opus-4-6","type":"model","display_name":"Claude Opus 4.6"},
 {"id":"claude-sonnet-4-6","type":"model","display_name":"Claude Sonnet 4.6"}
],"has_more":false,"first_id":"claude-opus-4-6","last_id":"claude-sonnet-4-6"}`

func TestListModelsSendsAPIKeyAndParsesIDs(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cannedModelsListResponse))
	}))
	defer srv.Close()

	lister := NewModelLister(config.Anthropic{APIKey: "sk-test-key", BaseURL: srv.URL})
	got, err := lister.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if gotKey != "sk-test-key" {
		t.Errorf("x-api-key = %q, want sk-test-key", gotKey)
	}
	if gotVersion != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicAPIVersion)
	}

	want := []string{"claude-opus-4-6", "claude-sonnet-4-6"}
	if len(got) != len(want) {
		t.Fatalf("ListModels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListModels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestListModelsWithoutAPIKeyReturnsErrNoAPIKeyWithoutACall pins the
// task brief's exact rule: "no key ⇒ cannot enumerate" — and that this
// must be detected WITHOUT ever making the HTTP call (a server that
// would fail the test if hit at all proves that).
func TestListModelsWithoutAPIKeyReturnsErrNoAPIKeyWithoutACall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	lister := NewModelLister(config.Anthropic{APIKey: "", BaseURL: srv.URL})
	_, err := lister.ListModels(context.Background())
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("ListModels err = %v, want ErrNoAPIKey", err)
	}
	if called {
		t.Error("ListModels hit the network with no API key configured — must short-circuit instead")
	}
}

func TestListModelsErrorsOnNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	lister := NewModelLister(config.Anthropic{APIKey: "sk-bad-key", BaseURL: srv.URL})
	if _, err := lister.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels = nil error, want one for a 401 response")
	}
}
