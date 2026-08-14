// Package prompts renders the versioned prompt templates AI
// capabilities use to talk to a language model. Every prompt is a
// pair of embedded text/template files — <name>.<version>.system.md
// and <name>.<version>.user.md — under templates/. Embedding pins the
// exact prompt text to the binary that shipped it: a prompt's wording
// is as load-bearing as code, so it's versioned and reviewed the same
// way.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed templates/*.md
var templatesFS embed.FS

// Prompt is a rendered prompt, split the way a chat completion call
// wants it: System sets the model's role and constraints, User carries
// the actual request.
type Prompt struct{ System, User string }

// Render renders the named/versioned template pair against data.
// Fields data doesn't provide but the template references are a hard
// error (missingkey=error) rather than a silently blank "<no value>" —
// a prompt with a dropped field is a broken prompt, not a degraded one.
func Render(name, version string, data any) (Prompt, error) {
	system, err := renderFile(name, version, "system", data)
	if err != nil {
		return Prompt{}, err
	}
	user, err := renderFile(name, version, "user", data)
	if err != nil {
		return Prompt{}, err
	}
	return Prompt{System: system, User: user}, nil
}

func renderFile(name, version, half string, data any) (string, error) {
	path := fmt.Sprintf("templates/%s.%s.%s.md", name, version, half)
	raw, err := templatesFS.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("prompts: unknown template %s/%s (%s): %w", name, version, half, err)
	}
	tmpl, err := template.New(path).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("prompts: parse %s: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("prompts: render %s: %w", path, err)
	}
	return buf.String(), nil
}
