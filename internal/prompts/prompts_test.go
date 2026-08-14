package prompts

import (
	"strings"
	"testing"
)

// teacherFeedbackData mirrors the fields the teacher.feedback.v1
// templates reference across both the system and user halves.
type teacherFeedbackData struct {
	TeacherMode         string
	Strictness          string
	ExplanationLanguage string
	Purpose             string
	Audience            string
	Register            string
	Context             string
	Selection           string
	RecentErrors        []string
}

func TestRenderTeacherFeedbackV1(t *testing.T) {
	data := teacherFeedbackData{
		TeacherMode:         "encouraging",
		Strictness:          "moderate",
		ExplanationLanguage: "en",
		Purpose:             "Diary entry",
		Audience:            "self",
		Register:            "casual",
		Context:             "今日はいい天気でした。",
		Selection:           "友達と公園に行きました。",
		RecentErrors:        []string{"particle を/に confusion", "い-adjective past tense"},
	}

	p, err := Render("teacher.feedback", "v1", data)
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	if p.System == "" {
		t.Fatal("System is empty")
	}
	if p.User == "" {
		t.Fatal("User is empty")
	}
	for _, want := range []string{"encouraging", "moderate", "en"} {
		if !strings.Contains(p.System, want) {
			t.Errorf("System = %q, want it to contain %q", p.System, want)
		}
	}
	if !strings.Contains(p.User, data.Selection) {
		t.Errorf("User = %q, want it to contain selection %q", p.User, data.Selection)
	}
	if !strings.Contains(p.User, "Diary entry") {
		t.Errorf("User = %q, want it to contain purpose %q", p.User, "Diary entry")
	}
	for _, want := range data.RecentErrors {
		if !strings.Contains(p.User, want) {
			t.Errorf("User = %q, want it to contain recent error %q", p.User, want)
		}
	}
}

func TestRenderUnknownNameErrors(t *testing.T) {
	if _, err := Render("no.such.prompt", "v1", struct{}{}); err == nil {
		t.Fatal("expected error for unknown prompt name, got nil")
	}
}

func TestRenderUnknownVersionErrors(t *testing.T) {
	if _, err := Render("teacher.feedback", "v99", struct{}{}); err == nil {
		t.Fatal("expected error for unknown prompt version, got nil")
	}
}

func TestRenderMissingFieldErrors(t *testing.T) {
	// missingkey=error: a data value that lacks a field the template
	// references must fail loudly rather than silently render "<no value>".
	type incomplete struct{ TeacherMode string }
	if _, err := Render("teacher.feedback", "v1", incomplete{TeacherMode: "x"}); err == nil {
		t.Fatal("expected error for data missing template fields, got nil")
	}
}
