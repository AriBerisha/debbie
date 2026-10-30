package tools

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The matching here follows Qwen Code's edit pipeline
// (packages/core/src/utils/editHelper.ts): the exact text first, then with
// typographic lookalike characters made plain, then line by line ignoring
// trailing whitespace, never relaxing indentation along the way. Unlike
// Qwen Code, every loose match is counted, so a second place that matches
// makes the edit ambiguous instead of silently losing. Last resorts for
// small local models are added: text whose indentation was shifted
// consistently and over-escaped text (a literal \n for a newline), each
// accepted only when it matches exactly one place, and line numbers copied
// from read_file's output along with the text.

// lookalikes maps characters that models and editors swap for one another
// to plain ASCII: dashes, curly quotes and unusual spaces.
var lookalikes = map[rune]rune{
	0x2010: '-', 0x2011: '-', 0x2012: '-', 0x2013: '-', 0x2014: '-', 0x2015: '-', 0x2212: '-',
	0x2018: '\'', 0x2019: '\'', 0x201A: '\'', 0x201B: '\'',
	0x201C: '"', 0x201D: '"', 0x201E: '"', 0x201F: '"',
	0x00A0: ' ', 0x2002: ' ', 0x2003: ' ', 0x2004: ' ', 0x2005: ' ', 0x2006: ' ', 0x2007: ' ',
	0x2008: ' ', 0x2009: ' ', 0x200A: ' ', 0x202F: ' ', 0x205F: ' ', 0x3000: ' ',
}

// plain maps lookalike characters to ASCII. For each byte of the result it
// also returns the offset in s of the byte it came from, plus len(s).
func plain(s string) (string, []int) {
	var b strings.Builder
	pos := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if a, ok := lookalikes[r]; ok {
			b.WriteRune(a)
			pos = append(pos, i)
		} else {
			b.WriteString(s[i : i+size])
			for k := range size {
				pos = append(pos, i+k)
			}
		}
		i += size
	}
	return b.String(), append(pos, len(s))
}

// editMatch is one place where old_string matched.
type editMatch struct {
	start, end int    // byte offsets in the text
	line       int    // 1-based line of start
	repl       string // what replaces text[start:end]
}

// editError is a matching error whose message the model sees as it is.
type editError struct{ msg string }

func (e *editError) Error() string { return e.msg }

// errNoMatch means old_string matched nowhere; the caller explains.
var errNoMatch = errors.New("no match")

// matchEdit finds where old matches text, both with LF line endings, and
// what to put there. With replaceAll every match counts; otherwise exactly
// one must match.
func matchEdit(text, old, repl string, replaceAll bool) ([]editMatch, error) {
	spans := lineSpans(text)
	strategies := []func() []editMatch{
		func() []editMatch { return exactMatches(text, old, repl, spans) },
		func() []editMatch { return plainMatches(text, old, repl, spans) },
	}
	for _, pass := range linePasses {
		strategies = append(strategies, func() []editMatch { return lineMatches(text, spans, old, repl, pass) })
	}
	for _, find := range strategies {
		if m := find(); len(m) > 0 {
			if len(m) > 1 && !replaceAll {
				return nil, ambiguous(m)
			}
			return m, nil
		}
	}
	// Last resorts: unique matches only.
	if m := indentMatches(text, spans, old, repl); len(m) == 1 && !replaceAll {
		return m, nil
	}
	if uold, urepl, ok := unescape(old, repl); ok {
		if m, err := matchEdit(text, uold, urepl, false); err == nil {
			return m, nil
		}
	}
	if sold, srepl, ok := stripLineNumbers(old, repl); ok {
		return matchEdit(text, sold, srepl, replaceAll)
	}
	return nil, errNoMatch
}

// lineNumberRE matches the line numbers read_file puts before each line.
var lineNumberRE = regexp.MustCompile(`^ *\d+\t`)

// stripLineNumbers removes line numbers copied from read_file's output
// along with the text: from old only when every line of it has one, and
// then from the lines of repl that have one.
func stripLineNumbers(old, repl string) (string, string, bool) {
	for _, l := range strings.Split(strings.TrimSuffix(old, "\n"), "\n") {
		if !lineNumberRE.MatchString(l) {
			return "", "", false
		}
	}
	strip := func(s string) string {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			lines[i] = lineNumberRE.ReplaceAllString(l, "")
		}
		return strings.Join(lines, "\n")
	}
	return strip(old), strip(repl), true
}

func ambiguous(m []editMatch) error {
	var lines []string
	for i, x := range m {
		if i == 10 {
			lines = append(lines, "…")
			break
		}
		lines = append(lines, fmt.Sprint(x.line))
	}
	return &editError{fmt.Sprintf("old_string matches %d places (lines %s); include more of the surrounding lines so it matches one, or set replace_all to true", len(m), strings.Join(lines, ", "))}
}

func exactMatches(text, old, repl string, spans []span) []editMatch {
	var m []editMatch
	for i := 0; ; {
		j := strings.Index(text[i:], old)
		if j < 0 {
			return m
		}
		start := i + j
		m = append(m, editMatch{start: start, end: start + len(old), line: lineAt(spans, start), repl: repl})
		i = start + len(old)
	}
}

func plainMatches(text, old, repl string, spans []span) []editMatch {
	ptext, pos := plain(text)
	pold, _ := plain(old)
	if ptext == text && pold == old {
		return nil // nothing to normalize: the exact search already failed
	}
	var m []editMatch
	for i := 0; ; {
		j := strings.Index(ptext[i:], pold)
		if j < 0 {
			return m
		}
		start := i + j
		m = append(m, editMatch{start: pos[start], end: pos[start+len(pold)], line: lineAt(spans, pos[start]), repl: repl})
		i = start + len(pold)
	}
}

// span is a line of the text, without its newline.
type span struct{ start, end int }

func lineSpans(text string) []span {
	var spans []span
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			spans = append(spans, span{start, i})
			start = i + 1
		}
	}
	if start < len(text) {
		spans = append(spans, span{start, len(text)})
	}
	return spans
}

func lineAt(spans []span, offset int) int {
	return sort.Search(len(spans), func(i int) bool { return spans[i].end >= offset }) + 1
}

// linePasses compare lines ever more loosely: as they are, without
// trailing whitespace, and also with lookalike characters made plain.
var linePasses = []func(string) string{
	func(s string) string { return s },
	func(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) },
	func(s string) string { p, _ := plain(s); return strings.TrimRightFunc(p, unicode.IsSpace) },
}

// lineMatches finds the places where every line of old equals a line of
// the text once both go through same.
func lineMatches(text string, spans []span, old, repl string, same func(string) string) []editMatch {
	want := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	var m []editMatch
	for i := 0; i+len(want) <= len(spans); i++ {
		ok := true
		for j, w := range want {
			if same(text[spans[i+j].start:spans[i+j].end]) != same(w) {
				ok = false
				break
			}
		}
		if ok {
			m = append(m, wholeLines(text, spans, i, len(want), old, repl))
			i += len(want) - 1
		}
	}
	return m
}

// wholeLines builds the match for n lines starting at line i. When old ends
// in a newline, the match takes the newline after its last line too; at the
// end of a file without a final newline it can't, so the replacement loses
// its final newline instead.
func wholeLines(text string, spans []span, i, n int, old, repl string) editMatch {
	m := editMatch{start: spans[i].start, end: spans[i+n-1].end, line: i + 1, repl: repl}
	if strings.HasSuffix(old, "\n") {
		if m.end < len(text) && text[m.end] == '\n' {
			m.end++
		} else {
			m.repl = strings.TrimSuffix(repl, "\n")
		}
	}
	return m
}

// indentMatches matches lines ignoring their indentation, as long as the
// file's indentation differs from old's by the same prefix on every line.
// The replacement is re-indented by that difference.
func indentMatches(text string, spans []span, old, repl string) []editMatch {
	want := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	bare := func(s string) string { p, _ := plain(s); return strings.TrimSpace(p) }
	var m []editMatch
	for i := 0; i+len(want) <= len(spans); i++ {
		var got []string
		ok := true
		for j, w := range want {
			line := text[spans[i+j].start:spans[i+j].end]
			if bare(line) != bare(w) {
				ok = false
				break
			}
			got = append(got, line)
		}
		if !ok {
			continue
		}
		if fix, ok := reindenter(got, want); ok {
			mm := wholeLines(text, spans, i, len(want), old, fix(repl))
			m = append(m, mm)
		}
	}
	return m
}

// reindenter works out how the file's lines are indented relative to the
// model's: by a common extra prefix, or by a common missing prefix. It
// returns a function applying the same change to other text.
func reindenter(file, model []string) (func(string) string, bool) {
	indent := func(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }
	var added, removed *string
	for i := range file {
		if strings.TrimSpace(file[i]) == "" {
			continue
		}
		fi, mi := indent(file[i]), indent(model[i])
		switch {
		case strings.HasPrefix(fi, mi) && len(fi) > len(mi):
			d := fi[:len(fi)-len(mi)]
			if added != nil && *added != d || removed != nil {
				return nil, false
			}
			added = &d
		case strings.HasPrefix(mi, fi) && len(mi) > len(fi):
			d := mi[:len(mi)-len(fi)]
			if removed != nil && *removed != d || added != nil {
				return nil, false
			}
			removed = &d
		case fi != mi:
			return nil, false
		}
	}
	if added == nil && removed == nil {
		return nil, false // same indentation: the earlier passes would have matched
	}
	return func(s string) string {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if added != nil {
				lines[i] = *added + l
			} else {
				lines[i] = strings.TrimPrefix(l, *removed)
			}
		}
		return strings.Join(lines, "\n")
	}, true
}

// unescape undoes over-escaping: old written as one line with literal \n,
// \t, \" sequences where the file has newlines, tabs and quotes. The
// replacement is unescaped the same way.
func unescape(old, repl string) (string, string, bool) {
	if strings.Contains(old, "\n") || !strings.Contains(old, `\`) {
		return "", "", false
	}
	r := strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\t`, "\t", `\"`, `"`, `\'`, `'`)
	u := r.Replace(old)
	if u == old {
		return "", "", false
	}
	return u, r.Replace(repl), true
}

// applyMatches replaces the matches, which must not overlap, in text.
func applyMatches(text string, m []editMatch) string {
	var b strings.Builder
	last := 0
	for _, x := range m {
		b.WriteString(text[last:x.start])
		b.WriteString(x.repl)
		last = x.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// closestLines lists the lines of text most like the first line of old that
// has any text, for the no-match error.
func closestLines(text, old string) string {
	var first string
	for _, l := range strings.Split(old, "\n") {
		if strings.TrimSpace(l) != "" {
			first = l
			break
		}
	}
	type scored struct {
		line  int
		score float64
		text  string
	}
	var best []scored
	for i, l := range strings.Split(text, "\n") {
		if s := similarity(first, l); s >= 0.5 {
			best = append(best, scored{i + 1, s, l})
		}
	}
	if len(best) == 0 {
		return ""
	}
	sort.SliceStable(best, func(i, j int) bool { return best[i].score > best[j].score })
	var b strings.Builder
	b.WriteString("\nThe lines most like the start of old_string:")
	for _, s := range best[:min(3, len(best))] {
		fmt.Fprintf(&b, "\n%6d\t%s", s.line, s.text)
	}
	return b.String()
}

// similarity is the Dice coefficient of the two trimmed lines' byte pairs.
func similarity(a, b string) float64 {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return 1
	}
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	pairs := map[string]int{}
	for i := 0; i+1 < len(a); i++ {
		pairs[a[i:i+2]]++
	}
	common := 0
	for i := 0; i+1 < len(b); i++ {
		if pairs[b[i:i+2]] > 0 {
			pairs[b[i:i+2]]--
			common++
		}
	}
	return 2 * float64(common) / float64(len(a)+len(b)-2)
}
