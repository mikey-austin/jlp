// Package extension embeds the Chrome extension's shared capture files so
// the server can serve them to JLP's phone pages and build the
// bookmarklet from them: one capture implementation for desktop and
// phone. Only article.js and imaging.js are exposed.
package extension

import (
	"embed"
	"strings"
)

//go:embed article.js imaging.js
var files embed.FS

// File returns one embedded shared file.
func File(name string) ([]byte, bool) {
	if name != "article.js" && name != "imaging.js" {
		return nil, false
	}
	b, err := files.ReadFile(name)
	return b, err == nil
}

// ArticleCapture returns article.js as a bare expression for inlining
// into the bookmarklet: whole-line // comments removed (they would be
// noise in a URL), and the trailing semicolon dropped so it can sit on
// the right of an assignment.
func ArticleCapture() string {
	b, _ := files.ReadFile("article.js")
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	src := strings.Join(out, "\n")
	return strings.TrimSuffix(strings.TrimSpace(src), ";")
}
