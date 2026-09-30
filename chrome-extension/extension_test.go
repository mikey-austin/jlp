package extension

import (
	"strings"
	"testing"
)

func TestFilesAreEmbedded(t *testing.T) {
	for _, name := range []string{"article.js", "imaging.js"} {
		b, ok := File(name)
		if !ok || len(b) == 0 {
			t.Fatalf("%s not embedded", name)
		}
	}
	if _, ok := File("popup.js"); ok {
		t.Fatal("only the shared capture files are served")
	}
}

func TestArticleCaptureIsAnExpression(t *testing.T) {
	src := ArticleCapture()
	if !strings.HasPrefix(src, "(function") || strings.HasSuffix(src, ";") || !strings.HasSuffix(src, ")") {
		t.Fatalf("capture must be the bare IIFE expression; got %q…%q", src[:20], src[len(src)-20:])
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			t.Fatalf("whole-line comment survived: %q", line)
		}
	}
	if !strings.Contains(src, "figures") {
		t.Fatal("not the article capture")
	}
}
