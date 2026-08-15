// Package ankiconnect implements a client for AnkiConnect
// (https://foosoft.net/projects/anki-connect/), the JSON-RPC-over-HTTP
// add-on that lets an external application talk to a running local Anki
// instance. It backs application/anki.Service.PushToAnkiConnect's
// optional "send straight into Anki" export path (PRD §19) — the
// alternative to (and independent of) the TSV file export.
package ankiconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

const (
	// ankiConnectVersion is AnkiConnect's own API version, sent on every
	// request per its protocol — see
	// https://foosoft.net/projects/anki-connect/#sample-invocation.
	ankiConnectVersion = 6
	deckName           = "JLP"
	modelName          = "Basic"
	cardTag            = "jlp"
)

// Client talks to one AnkiConnect endpoint (typically
// http://localhost:8765, Anki's default when the add-on is installed
// and running).
type Client struct {
	url    string
	client *http.Client
}

// New returns a Client for the AnkiConnect server at url.
func New(url string) *Client {
	return &Client{url: url, client: &http.Client{}}
}

// noteFields mirrors AnkiConnect's addNotes note.fields object for the
// "Basic" note type: Front/Back, capitalized, matching that model's own
// field names.
type noteFields struct {
	Front string `json:"Front"`
	Back  string `json:"Back"`
}

// note mirrors one entry of AnkiConnect's addNotes params.notes array.
type note struct {
	DeckName  string     `json:"deckName"`
	ModelName string     `json:"modelName"`
	Fields    noteFields `json:"fields"`
	Tags      []string   `json:"tags"`
}

// addNotesParams mirrors AnkiConnect's addNotes action params object.
type addNotesParams struct {
	Notes []note `json:"notes"`
}

// request mirrors AnkiConnect's JSON-RPC-shaped request envelope: every
// action takes {"action":..., "version":6, "params":...}.
type request struct {
	Action  string         `json:"action"`
	Version int            `json:"version"`
	Params  addNotesParams `json:"params"`
}

// response mirrors AnkiConnect's addNotes response: Result is a
// parallel array of note IDs (one per input note, in order), with null
// for any note Anki rejected (e.g. a duplicate); Error is a
// human-readable message set when the WHOLE request failed (as opposed
// to a per-note rejection, which shows up as a null in Result instead).
type response struct {
	Result []*int64 `json:"result"`
	Error  *string  `json:"error"`
}

// AddNotes sends cards to Anki as one addNotes batch call and reports
// how many were actually added (a null entry in AnkiConnect's Result —
// a duplicate note, most commonly — does not count). A non-nil
// response.Error (the whole request failed, distinct from an individual
// note being rejected) is returned as an error.
func (c *Client) AddNotes(ctx context.Context, cards []storage.AnkiCard) (added int, err error) {
	notes := make([]note, 0, len(cards))
	for _, card := range cards {
		notes = append(notes, note{
			DeckName:  deckName,
			ModelName: modelName,
			Fields:    noteFields{Front: card.Front, Back: card.Back},
			Tags:      []string{cardTag},
		})
	}

	body := request{
		Action:  "addNotes",
		Version: ankiConnectVersion,
		Params:  addNotesParams{Notes: notes},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("ankiconnect: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("ankiconnect: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("ankiconnect: addNotes: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return 0, fmt.Errorf("ankiconnect: read response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return 0, fmt.Errorf("ankiconnect: addNotes: status %d: %s", httpResp.StatusCode, raw)
	}

	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, fmt.Errorf("ankiconnect: unmarshal response: %w", err)
	}
	if resp.Error != nil {
		return 0, fmt.Errorf("ankiconnect: addNotes: %s", *resp.Error)
	}

	for _, id := range resp.Result {
		if id != nil {
			added++
		}
	}
	return added, nil
}

// ensure the interface is satisfied at compile time.
var _ interface {
	AddNotes(ctx context.Context, cards []storage.AnkiCard) (int, error)
} = (*Client)(nil)
