package cover

import (
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// noBreakBefore are characters a Japanese line must not start with
// (kinsoku shori, the common subset).
const noBreakBefore = "、。，．」』）】〕！？ー…・：；"

func textWidth(face font.Face, s string) fixed.Int26_6 { return font.MeasureString(face, s) }

// wrapLines breaks s into lines no wider than width, at any character
// (Japanese has no spaces to wait for) but never starting a line with a
// no-break-before character, and ellipsises the last of maxLines.
func wrapLines(face font.Face, s string, width fixed.Int26_6, maxLines int) []string {
	var lines []string
	var cur []rune
	for _, r := range strings.TrimSpace(s) {
		next := append(append([]rune{}, cur...), r)
		if len(cur) > 0 && textWidth(face, string(next)) > width {
			// With a single rune there is nothing to pull down without
			// emitting an empty line, so break normally.
			if strings.ContainsRune(noBreakBefore, r) && len(cur) >= 2 {
				// Pull the last character down with this one.
				last := cur[len(cur)-1]
				lines = append(lines, string(cur[:len(cur)-1]))
				cur = []rune{last, r}
			} else {
				lines = append(lines, string(cur))
				cur = []rune{r}
			}
			continue
		}
		cur = next
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	if len(lines) <= maxLines {
		return lines
	}
	lines = lines[:maxLines]
	last := []rune(lines[maxLines-1])
	for len(last) > 0 && textWidth(face, string(last)+"…") > width {
		last = last[:len(last)-1]
	}
	lines[maxLines-1] = string(last) + "…"
	return lines
}
