package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
)

// liveTagsResponse is /api/tags' real shape, captured live on this
// machine (task brief): five completion-capable chat models plus two
// mxbai-embed-large entries whose ONLY capability is "embedding" — the
// exact case ListModels must filter out.
const liveTagsResponse = `{"models":[
 {"name":"gemma4:12b","model":"gemma4:12b","details":{"family":"gemma4"},"capabilities":["completion","tools","thinking","vision"]},
 {"name":"gemma4:31b","model":"gemma4:31b","details":{"family":"gemma4"},"capabilities":["completion","tools","thinking"]},
 {"name":"gemma4:latest","model":"gemma4:latest","details":{"family":"gemma4"},"capabilities":["completion","tools","thinking"]},
 {"name":"qwen3.6:35b","model":"qwen3.6:35b","details":{"family":"qwen35moe"},"capabilities":["vision","completion","tools","thinking"]},
 {"name":"qwen3.6:latest","model":"qwen3.6:latest","details":{"family":"qwen35moe"},"capabilities":["vision","completion","tools","thinking"]},
 {"name":"mxbai-embed-large:latest","model":"mxbai-embed-large:latest","details":{"family":"bert"},"capabilities":["embedding"]},
 {"name":"mxbai-embed-large:335m","model":"mxbai-embed-large:335m","details":{"family":"bert"},"capabilities":["embedding"]}
]}`

func TestListModelsFiltersToCompletionCapable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveTagsResponse))
	}))
	defer srv.Close()

	lister := NewModelLister(config.Ollama{URL: srv.URL})
	got, err := lister.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	want := []string{"gemma4:12b", "gemma4:31b", "gemma4:latest", "qwen3.6:35b", "qwen3.6:latest"}
	if len(got) != len(want) {
		t.Fatalf("ListModels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListModels[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, name := range got {
		if name == "mxbai-embed-large:latest" || name == "mxbai-embed-large:335m" {
			t.Errorf("ListModels included embedding-only model %q", name)
		}
	}
}

func TestListModelsErrorsOnNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	lister := NewModelLister(config.Ollama{URL: srv.URL})
	if _, err := lister.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels = nil error, want one for a 500 response")
	}
}

// TestListModelsErrorsWhenUnreachable pins the "degrade, don't
// fabricate" contract at the transport level: a dead port must produce
// an error, never a silently empty (and therefore misleadingly
// "there really are zero models") result.
func TestListModelsErrorsWhenUnreachable(t *testing.T) {
	lister := NewModelLister(config.Ollama{URL: "http://127.0.0.1:1"})
	if _, err := lister.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels = nil error, want one when the server is unreachable")
	}
}
