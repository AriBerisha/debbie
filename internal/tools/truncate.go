package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Output limits for anything a tool returns to the model (SPEC §8.4).
const (
	MaxOutputChars = 30_000
	MaxOutputLines = 2_000
)

// Cap shortens tool output to MaxOutputLines lines and MaxOutputChars
// characters, keeping its start and end and saying in the middle how much
// was left out.
func Cap(s string) string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= MaxOutputLines && utf8.RuneCountInString(s) <= MaxOutputChars {
		return s
	}
	head, tail := lines, []string(nil)
	if len(lines) > MaxOutputLines {
		keep := MaxOutputLines - 1 // one line for the marker
		head, tail = lines[:keep/2], lines[len(lines)-(keep-keep/2):]
	}
	h, t := strings.Join(head, ""), strings.Join(tail, "")
	const budget = MaxOutputChars - 100 // room for the marker
	if utf8.RuneCountInString(h)+utf8.RuneCountInString(t) > budget {
		h, t = firstRunes(h, budget/2), lastRunes(t, budget-budget/2)
		if t == "" { // everything was in one long head
			t = lastRunes(s, budget-budget/2)
		}
	}
	omittedChars := utf8.RuneCountInString(s) - utf8.RuneCountInString(h) - utf8.RuneCountInString(t)
	omittedLines := strings.Count(s, "\n") - strings.Count(h, "\n") - strings.Count(t, "\n")
	if !strings.HasSuffix(h, "\n") {
		h += "\n"
	}
	return h + fmt.Sprintf("[... %d lines, %d characters left out ...]\n", max(omittedLines, 0), omittedChars) + t
}

func firstRunes(s string, n int) string {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

func lastRunes(s string, n int) string {
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

// Lines says how many lines there are: "1 line", "3 lines".
func Lines(n int) string { return Count(n, "line", "lines") }

// Count says how many there are of something: "1 file", "3 files".
func Count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
