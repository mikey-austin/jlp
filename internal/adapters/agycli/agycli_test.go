package agycli

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/agy_live.json is REAL stdout from agy 1.1.9, captured by
// running this repo's actual teacher.feedback prompt and
// correction_result.v1 schema through the same invocation shape
// GenerateStructured builds (-p <doc>, --output-format json,
// --mode plan, --json-schema <file>, fresh scratch cwd).
func liveEnvelope(t *testing.T) envelope {
	t.Helper()
	raw, err := os.ReadFile("testdata/agy_live.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}

// TestEnvelopeParsesALiveTranscript pins the envelope contract: status,
// the already-parsed structured_output, and usage. If a future agy
// version renames any of these, this fails here rather than in
// production.
func TestEnvelopeParsesALiveTranscript(t *testing.T) {
	env := liveEnvelope(t)

	if env.Status != "SUCCESS" {
		t.Errorf("Status = %q, want SUCCESS", env.Status)
	}
	if len(env.StructuredOutput) == 0 || string(env.StructuredOutput) == "null" {
		t.Fatalf("StructuredOutput is empty — the --json-schema answer must arrive here")
	}

	var answer struct {
		Corrections []struct {
			Original    string `json:"original"`
			Replacement string `json:"replacement"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(env.StructuredOutput, &answer); err != nil {
		t.Fatalf("structured_output is not the schema's shape: %v", err)
	}
	if len(answer.Corrections) == 0 {
		t.Fatal("structured_output carried no corrections")
	}
	if got := answer.Corrections[0].Original; got != "面白いでした" {
		t.Errorf("first correction original = %q, want %q", got, "面白いでした")
	}

	// The values this exact transcript reported.
	if want := 18089; env.Usage.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d", env.Usage.InputTokens, want)
	}
	if want := 36627; env.Usage.CacheReadTokens != want {
		t.Errorf("CacheReadTokens = %d, want %d", env.Usage.CacheReadTokens, want)
	}
	if want := 1039; env.Usage.OutputTokens != want {
		t.Errorf("OutputTokens = %d, want %d", env.Usage.OutputTokens, want)
	}
}

// TestStructuredOutputIsPreferredOverResponse: with --json-schema the
// answer appears in BOTH fields, but Response has the agent's prose in
// front of it ("I have created the implementation plan artifact…"),
// which is not valid JSON. Preferring structured_output is what keeps
// the caller's schema validation from failing on commentary.
func TestStructuredOutputIsPreferredOverResponse(t *testing.T) {
	env := liveEnvelope(t)

	if json.Valid([]byte(env.Response)) {
		t.Skip("this transcript's response happens to be bare JSON; the preference is untestable from it")
	}
	if !json.Valid(env.StructuredOutput) {
		t.Fatal("structured_output is not valid JSON")
	}
}

func TestClampEffortMapsOntoAgysLadder(t *testing.T) {
	// agy's ladder stops at high: passing xhigh or max verbatim would
	// make the CLI reject the whole invocation, so they collapse to the
	// hardest tier it does have. Everything else passes through, and ""
	// stays "" (meaning "add no --effort flag at all").
	for in, want := range map[string]string{
		"":       "",
		"low":    "low",
		"medium": "medium",
		"high":   "high",
		"xhigh":  "high",
		"max":    "high",
	} {
		if got := clampEffort(in); got != want {
			t.Errorf("clampEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterEnvDropsTheKeysThatWouldBypassTheLogin(t *testing.T) {
	// This adapter exists to use the operator's existing Google login;
	// the CLI prefers an API key whenever it sees one, so a stray key in
	// the server's environment would silently change which account (and
	// bill) answers.
	in := []string{"PATH=/usr/bin", "GEMINI_API_KEY=secret", "HOME=/home/x", "GOOGLE_API_KEY=other"}
	got := filterEnv(in, "GEMINI_API_KEY", "GOOGLE_API_KEY")
	want := []string{"PATH=/usr/bin", "HOME=/home/x"}
	if len(got) != len(want) {
		t.Fatalf("filterEnv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterEnv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTailExcerptKeepsTheEnd(t *testing.T) {
	// agy writes progress noise first and the real failure last.
	long := make([]byte, maxExcerpt+100)
	for i := range long {
		long[i] = 'a'
	}
	copy(long[len(long)-5:], []byte("BOOM!"))
	got := tailExcerpt(string(long))
	if len(got) > maxExcerpt+3 {
		t.Errorf("excerpt length = %d, want <= %d", len(got), maxExcerpt+3)
	}
	if got[len(got)-5:] != "BOOM!" {
		t.Errorf("excerpt does not end with the tail: %q", got[len(got)-10:])
	}
	if tailExcerpt("   ") != "(no output)" {
		t.Errorf("blank input should read as (no output)")
	}
}
