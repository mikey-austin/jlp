package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus" //nolint:depguard // port-shaped test double; see fakes_test.go's package doc comment
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/tools"
)

func TestGetRecentWritingDelegatesAndReturnsCompactShape(t *testing.T) {
	docs := newFakeDocumentRepo()
	docs.seed(testIdentity, "s-1", strings.Repeat("あ", 50))
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	svc := appwriting.NewService(docs, rec)

	tool := findTool(t, tools.WritingTools(svc), "get_recent_writing")
	out, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"session_id":"s-1"}`))
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["rune_count"].(float64) != 50 {
		t.Fatalf("rune_count = %v, want 50: %s", got["rune_count"], out)
	}
	if got["truncated"] != false {
		t.Fatalf("truncated = %v, want false for 50 runes: %s", got["truncated"], out)
	}
}

func TestGetRecentWritingTruncatesLongDocuments(t *testing.T) {
	docs := newFakeDocumentRepo()
	docs.seed(testIdentity, "s-1", strings.Repeat("あ", 5000))
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	svc := appwriting.NewService(docs, rec)

	tool := findTool(t, tools.WritingTools(svc), "get_recent_writing")
	out, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{"session_id":"s-1"}`))
	if err != nil {
		t.Fatalf("Handler() err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["truncated"] != true {
		t.Fatalf("truncated = %v, want true for 5000 runes (keeps tool output compact)", got["truncated"])
	}
	if preview, ok := got["preview"].(string); !ok || len([]rune(preview)) >= 5000 {
		t.Fatalf("preview length = %d, want < 5000 (truncated)", len([]rune(got["preview"].(string))))
	}
}

func TestGetRecentWritingRequiresSessionID(t *testing.T) {
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	svc := appwriting.NewService(newFakeDocumentRepo(), rec)
	tool := findTool(t, tools.WritingTools(svc), "get_recent_writing")

	_, err := tool.Handler(context.Background(), testIdentity, nil, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Handler() err = nil, want an error when session_id is missing")
	}
}
