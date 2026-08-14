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
	"sync"
	"text/template"
)

//go:embed templates/*.md
var templatesFS embed.FS

// Prompt is a rendered prompt, split the way a chat completion call
// wants it: System sets the model's role and constraints, User carries
// the actual request.
type Prompt struct{ System, User string }

var (
	parseOnce     sync.Once
	parsedTmpls   map[string]*template.Template
	parseTmplsErr error
)

// Render renders the named/versioned template pair against data.
// Fields data doesn't provide but the template references are a hard
// error (missingkey=error) rather than a silently blank "<no value>" —
// a prompt with a dropped field is a broken prompt, not a degraded one.
func Render(name, version string, data any) (Prompt, error) {
	tmpls, err := parsedTemplates()
	if err != nil {
		return Prompt{}, fmt.Errorf("prompts: %w", err)
	}
	system, err := renderFile(tmpls, name, version, "system", data)
	if err != nil {
		return Prompt{}, err
	}
	user, err := renderFile(tmpls, name, version, "user", data)
	if err != nil {
		return Prompt{}, err
	}
	return Prompt{System: system, User: user}, nil
}

// parsedTemplates parses every embedded template exactly once (like
// package schemas compiles its JSON Schemas once) and caches the
// result: template text never changes at runtime, so there's no
// reason to re-read and re-parse it on every Render call.
func parsedTemplates() (map[string]*template.Template, error) {
	parseOnce.Do(func() {
		entries, err := templatesFS.ReadDir("templates")
		if err != nil {
			parseTmplsErr = err
			return
		}
		out := make(map[string]*template.Template, len(entries))
		for _, e := range entries {
			path := "templates/" + e.Name()
			raw, err := templatesFS.ReadFile(path)
			if err != nil {
				parseTmplsErr = err
				return
			}
			tmpl, err := template.New(path).Option("missingkey=error").Parse(string(raw))
			if err != nil {
				parseTmplsErr = fmt.Errorf("parse %s: %w", path, err)
				return
			}
			out[path] = tmpl
		}
		parsedTmpls = out
	})
	return parsedTmpls, parseTmplsErr
}

// renderFile executes the cached template for name/version/half
// (e.g. "teacher.feedback"/"v1"/"system") against data. A template
// can be executed concurrently by multiple goroutines as long as each
// call writes to its own buffer, which this does.
func renderFile(tmpls map[string]*template.Template, name, version, half string, data any) (string, error) {
	path := fmt.Sprintf("templates/%s.%s.%s.md", name, version, half)
	tmpl, ok := tmpls[path]
	if !ok {
		return "", fmt.Errorf("prompts: unknown template %s/%s (%s)", name, version, half)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("prompts: render %s: %w", path, err)
	}
	return buf.String(), nil
}
