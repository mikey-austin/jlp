package ankiconnect_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/ankiconnect"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// requestAssertion is a captured, decoded copy of the single HTTP
// request the adapter sent — mirrors adapters/ollama's own test style.
type requestAssertion struct {
	Method string
	Body   map[string]any
}

func newTestServer(t *testing.T, captured *requestAssertion, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request body is not valid JSON: %v\nbody: %s", err, raw)
		}
		captured.Body = body

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
}

func testCards() []storage.AnkiCard {
	return []storage.AnkiCard{
		{ID: "card-1", Front: "front text", Back: "back text"},
	}
}

// TestAddNotesSendsWireContractAndReportsAdded pins the brief's
// JSON-RPC body shape and the canned `{"result":[123],"error":null}`
// happy-path response.
func TestAddNotesSendsWireContractAndReportsAdded(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, `{"result":[123],"error":null}`)
	defer srv.Close()

	client := ankiconnect.New(srv.URL)
	added, err := client.AddNotes(context.Background(), testCards())
	if err != nil {
		t.Fatalf("AddNotes returned error: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}

	if captured.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.Method)
	}
	if captured.Body["action"] != "addNotes" {
		t.Errorf("action = %v, want addNotes", captured.Body["action"])
	}
	if captured.Body["version"] != float64(6) {
		t.Errorf("version = %v, want 6", captured.Body["version"])
	}
	params, ok := captured.Body["params"].(map[string]any)
	if !ok {
		t.Fatalf("params = %#v, want an object", captured.Body["params"])
	}
	notes, ok := params["notes"].([]any)
	if !ok || len(notes) != 1 {
		t.Fatalf("params.notes = %#v, want exactly 1 note", params["notes"])
	}
	n, ok := notes[0].(map[string]any)
	if !ok {
		t.Fatalf("notes[0] = %#v, want an object", notes[0])
	}
	if n["deckName"] != "JLP" {
		t.Errorf("deckName = %v, want JLP", n["deckName"])
	}
	if n["modelName"] != "Basic" {
		t.Errorf("modelName = %v, want Basic", n["modelName"])
	}
	fields, ok := n["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %#v, want an object", n["fields"])
	}
	if fields["Front"] != "front text" || fields["Back"] != "back text" {
		t.Errorf("fields = %+v, want Front/Back matching the card", fields)
	}
	tags, ok := n["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "jlp" {
		t.Errorf("tags = %#v, want [\"jlp\"]", n["tags"])
	}
}

// TestAddNotesCountsOnlyNonNullResults pins the "a null entry doesn't
// count as added" contract — AnkiConnect returns null for a note it
// rejected (e.g. a duplicate) alongside successfully-added ones in the
// same parallel array.
func TestAddNotesCountsOnlyNonNullResults(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, `{"result":[111,null,222],"error":null}`)
	defer srv.Close()

	client := ankiconnect.New(srv.URL)
	cards := []storage.AnkiCard{
		{ID: "1", Front: "f1", Back: "b1"},
		{ID: "2", Front: "f2", Back: "b2"},
		{ID: "3", Front: "f3", Back: "b3"},
	}
	added, err := client.AddNotes(context.Background(), cards)
	if err != nil {
		t.Fatalf("AddNotes returned error: %v", err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2 (one null result excluded)", added)
	}
}

// TestAddNotesSurfacesResponseLevelError pins AnkiConnect's
// whole-request error shape: a non-null "error" field must come back as
// a Go error, not be silently ignored.
func TestAddNotesSurfacesResponseLevelError(t *testing.T) {
	srv := newTestServer(t, &requestAssertion{}, `{"result":null,"error":"unsupported action"}`)
	defer srv.Close()

	client := ankiconnect.New(srv.URL)
	_, err := client.AddNotes(context.Background(), testCards())
	if err == nil {
		t.Fatal("expected an error for a non-null response.error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported action") {
		t.Errorf("error = %q, want it to mention the AnkiConnect error message", err.Error())
	}
}

// TestAddNotesWrapsNon2xxHTTPError pins transport-level failure
// handling — mirrors adapters/ollama's own equivalent test.
func TestAddNotesWrapsNon2xxHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`internal error`))
	}))
	defer srv.Close()

	client := ankiconnect.New(srv.URL)
	_, err := client.AddNotes(context.Background(), testCards())
	if err == nil {
		t.Fatal("expected an error for a non-2xx response, got nil")
	}
	if !strings.Contains(err.Error(), "ankiconnect") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "ankiconnect")
	}
}

// TestAddNotesEmptyCardsStillCallsAnkiConnect pins that an empty slice
// still sends a well-formed (empty notes array) request rather than
// short-circuiting — application/anki.Service already guards against
// calling AddNotes with nothing approved, so this is about the
// adapter's own robustness, not a path the service normally exercises.
func TestAddNotesEmptyCardsStillCallsAnkiConnect(t *testing.T) {
	var captured requestAssertion
	srv := newTestServer(t, &captured, `{"result":[],"error":null}`)
	defer srv.Close()

	client := ankiconnect.New(srv.URL)
	added, err := client.AddNotes(context.Background(), nil)
	if err != nil {
		t.Fatalf("AddNotes returned error: %v", err)
	}
	if added != 0 {
		t.Fatalf("added = %d, want 0", added)
	}
	params, ok := captured.Body["params"].(map[string]any)
	if !ok {
		t.Fatalf("params = %#v, want an object", captured.Body["params"])
	}
	notes, ok := params["notes"].([]any)
	if !ok || len(notes) != 0 {
		t.Fatalf("params.notes = %#v, want an empty array", params["notes"])
	}
}
