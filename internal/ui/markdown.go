package ui

import (
	"regexp"
	"strings"
	"unicode"
)

// Markdown in the fullscreen UI: a model's reply drawn with headings,
// emphasis, inline code, links, lists, quotes, rules, tables and fenced
// code blocks with simple keyword highlighting. Line breaks are kept as
// the model wrote them, since models rely on them.

var (
	codeLook    = look{fg: inkCyan}
	headLook    = look{fg: rgbInk(tealBright), bold: true}
	linkLook    = look{fg: inkCyan, underline: true}
	quoteLook   = look{fg: inkGray, italic: true}
	keywordLook = look{fg: inkCyan}
	stringLook  = look{fg: inkGreen}
	commentLook = look{fg: inkGray, italic: true}
	numberLook  = look{fg: inkYellow}
)

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	listRe    = regexp.MustCompile(`^(\s*)([-*+]|\d{1,3}[.)])\s+(.*)$`)
	ruleRe    = regexp.MustCompile(`^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	fenceRe   = regexp.MustCompile("^\\s{0,3}(```+|~~~+)\\s*([^`\\s]*)")
	tableSep  = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
)

// renderMarkdown lays out text in rows width columns wide.
func renderMarkdown(text string, width int) [][]span {
	width = max(4, width)
	lines := strings.Split(clean(text), "\n")
	var rows [][]span
	blank := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			fence := m[1]
			lang := strings.ToLower(m[2])
			var code []string
			for i++; i < len(lines); i++ {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), fence[:3]) && strings.TrimSpace(strings.Trim(strings.TrimSpace(lines[i]), fence[:1])) == "" {
					break
				}
				code = append(code, lines[i])
			}
			rows = append(rows, codeRows(code, lang, width)...)
			blank = false
			continue
		}
		if strings.TrimSpace(line) == "" {
			if !blank && len(rows) > 0 {
				rows = append(rows, nil)
			}
			blank = true
			continue
		}
		blank = false
		switch {
		case headingRe.MatchString(line):
			m := headingRe.FindStringSubmatch(line)
			l := headLook
			if len(m[1]) > 2 {
				l = look{bold: true}
			}
			rows = append(rows, wrap(restyle(inline(m[2]), l), width, nil, nil)...)
		case ruleRe.MatchString(line):
			rows = append(rows, []span{grayed(strings.Repeat("─", width))})
		case strings.HasPrefix(strings.TrimLeft(line, " "), ">"):
			body := strings.TrimPrefix(strings.TrimLeft(line, " "), ">")
			body = strings.TrimPrefix(body, " ")
			bar := []span{grayed("│ ")}
			rows = append(rows, wrap(restyle(inline(body), quoteLook), width, bar, bar)...)
		case listRe.MatchString(line):
			m := listRe.FindStringSubmatch(line)
			indent := strings.Repeat(" ", min(len(m[1]), 8))
			marker := m[2]
			if strings.ContainsAny(marker, "-*+") {
				marker = "•"
			}
			first := []span{plain(indent), inked(marker+" ", inkGray)}
			rest := []span{plain(indent + strings.Repeat(" ", textWidth(marker)+1))}
			rows = append(rows, wrap(inline(m[3]), width, first, rest)...)
		case isTableRow(line) && i+1 < len(lines) && tableSep.MatchString(lines[i+1]):
			var table []string
			for ; i < len(lines) && isTableRow(lines[i]); i++ {
				table = append(table, lines[i])
			}
			i--
			rows = append(rows, tableRows(table, width)...)
		default:
			rows = append(rows, wrap(inline(line), width, nil, nil)...)
		}
	}
	for len(rows) > 0 && rows[len(rows)-1] == nil {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// restyle adds l's attributes and color to spans without their own color.
func restyle(spans []span, l look) []span {
	out := make([]span, len(spans))
	for i, s := range spans {
		s.look.bold = s.look.bold || l.bold
		s.look.italic = s.look.italic || l.italic
		s.look.underline = s.look.underline || l.underline
		if s.look.fg.kind == inkDefault {
			s.look.fg = l.fg
		}
		out[i] = s
	}
	return out
}

// inline styles a line's emphasis, inline code and links.
func inline(s string) []span {
	var spans []span
	var b strings.Builder
	var cur look
	flush := func() {
		if b.Len() > 0 {
			spans = append(spans, span{b.String(), cur})
			b.Reset()
		}
	}
	rs := []rune(s)
	wordAt := func(i int) bool { return i >= 0 && i < len(rs) && (unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i])) }
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs) && strings.ContainsRune("\\`*_[]()#+-.!|>", rs[i+1]):
			b.WriteRune(rs[i+1])
			i++
		case r == '`':
			n := 1
			for i+n < len(rs) && rs[i+n] == '`' {
				n++
			}
			end := closingRun(rs, i+n, n)
			if end < 0 {
				b.WriteString(string(rs[i : i+n]))
				i += n - 1
				continue
			}
			flush()
			spans = append(spans, span{strings.TrimSpace(string(rs[i+n : end])), look{fg: codeLook.fg, bold: cur.bold}})
			i = end + n - 1
		case (r == '*' || r == '_') && i+1 < len(rs) && rs[i+1] == r:
			marker := string([]rune{r, r})
			if !cur.bold && !strings.Contains(string(rs[i+2:]), marker) {
				b.WriteString(marker)
				i++
				continue
			}
			flush()
			cur.bold = !cur.bold
			i++
		case r == '*' || r == '_' && !wordAt(i-1):
			closes := cur.italic && (r == '*' || !wordAt(i+1))
			opens := !cur.italic && i+1 < len(rs) && rs[i+1] != ' ' && strings.ContainsRune(string(rs[i+1:]), r)
			if !closes && !opens {
				b.WriteRune(r)
				continue
			}
			flush()
			cur.italic = !cur.italic
		case r == '[':
			closeText := indexRune(rs, i+1, ']')
			if closeText < 0 || closeText+1 >= len(rs) || rs[closeText+1] != '(' {
				b.WriteRune(r)
				continue
			}
			closeURL := indexRune(rs, closeText+2, ')')
			if closeURL < 0 {
				b.WriteRune(r)
				continue
			}
			text, url := string(rs[i+1:closeText]), string(rs[closeText+2:closeURL])
			flush()
			spans = append(spans, span{text, linkLook})
			if url != text {
				spans = append(spans, grayed(" ("+url+")"))
			}
			i = closeURL
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return spans
}

// indexRune finds r in rs at from or after it; -1 when it isn't there.
func indexRune(rs []rune, from int, r rune) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// closingRun finds a run of exactly n backticks in rs at from or after it,
// which closes an inline code span; -1 when there is none.
func closingRun(rs []rune, from, n int) int {
	for i := from; i+n <= len(rs); i++ {
		run := 0
		for i+run < len(rs) && rs[i+run] == '`' {
			run++
		}
		if run == n {
			return i
		}
		i += run
	}
	return -1
}

func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 2
}

// tableRows lays out a table in columns; one too wide for the width is
// drawn as its lines.
func tableRows(lines []string, width int) [][]span {
	var cells [][]string
	for i, l := range lines {
		if i == 1 {
			continue // the separator under the header
		}
		t := strings.Trim(strings.TrimSpace(l), "|")
		var row []string
		for _, c := range strings.Split(t, "|") {
			row = append(row, strings.TrimSpace(c))
		}
		cells = append(cells, row)
	}
	cols := 0
	for _, row := range cells {
		cols = max(cols, len(row))
	}
	widths := make([]int, cols)
	for _, row := range cells {
		for j, c := range row {
			widths[j] = max(widths[j], spansWidth(inline(c)))
		}
	}
	total := 0
	for _, w := range widths {
		total += w + 3
	}
	if total > width {
		var rows [][]span
		for _, l := range lines {
			rows = append(rows, wrap(inline(l), width, nil, nil)...)
		}
		return rows
	}
	var rows [][]span
	for i, row := range cells {
		var r []span
		for j := range cols {
			var c []span
			if j < len(row) {
				c = inline(row[j])
			}
			if i == 0 {
				c = restyle(c, look{bold: true})
			}
			if j > 0 {
				r = append(r, grayed(" │ "))
			}
			r = append(r, c...)
			r = append(r, plain(strings.Repeat(" ", widths[j]-spansWidth(c))))
		}
		rows = append(rows, r)
		if i == 0 {
			var sep strings.Builder
			for j, w := range widths {
				if j > 0 {
					sep.WriteString("─┼─")
				}
				sep.WriteString(strings.Repeat("─", w))
			}
			rows = append(rows, []span{grayed(sep.String())})
		}
	}
	return rows
}

// keywords of the languages code blocks get highlighted in.
var keywords = map[string]string{
	"go":         "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false",
	"python":     "and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield None True False self",
	"javascript": "async await break case catch class const continue debugger default delete do else export extends finally for function if import in instanceof let new of return super switch this throw try typeof var void while with yield null undefined true false",
	"rust":       "as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while",
	"c":          "auto break case char const continue default do double else enum extern float for goto if inline int long register return short signed sizeof static struct switch typedef union unsigned void volatile while class namespace public private protected template typename virtual new delete true false nullptr",
	"java":       "abstract boolean break byte case catch char class const continue default do double else enum extends final finally float for if implements import instanceof int interface long new package private protected public return short static super switch this throw throws try void volatile while true false null",
	"shell":      "if then else elif fi for while until do done case esac function in return export local echo exit",
	"json":       "true false null",
}

var langAliases = map[string]string{
	"golang": "go", "py": "python", "js": "javascript", "ts": "javascript", "typescript": "javascript", "jsx": "javascript",
	"tsx": "javascript", "rs": "rust", "cpp": "c", "c++": "c", "h": "c", "hpp": "c", "cs": "java", "csharp": "java",
	"kotlin": "java", "sh": "shell", "bash": "shell", "zsh": "shell", "powershell": "shell", "ps1": "shell", "yaml": "shell", "yml": "shell",
}

// codeRows draws a code block's lines, indented and highlighted, wrapped
// where they don't fit.
func codeRows(code []string, lang string, width int) [][]span {
	if a, ok := langAliases[lang]; ok {
		lang = a
	}
	words := map[string]bool{}
	for _, w := range strings.Fields(keywords[lang]) {
		words[w] = true
	}
	comment := "//"
	switch lang {
	case "python", "shell":
		comment = "#"
	}
	var rows [][]span
	indent := []span{grayed("  ")}
	for _, line := range code {
		rows = append(rows, wrap(highlight(line, words, comment), width, indent, indent)...)
	}
	return rows
}

// highlight colors one line of code: keywords, strings, comments and
// numbers.
func highlight(line string, words map[string]bool, comment string) []span {
	var spans []span
	rs := []rune(line)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case strings.HasPrefix(string(rs[i:]), comment):
			spans = append(spans, span{string(rs[i:]), commentLook})
			i = len(rs)
		case r == '"' || r == '\'' || r == '`':
			j := i + 1
			for j < len(rs) && rs[j] != r {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(rs))
			spans = append(spans, span{string(rs[i:j]), stringLook})
			i = j
		case unicode.IsDigit(r) && (i == 0 || !isWordRune(rs[i-1])):
			j := i
			for j < len(rs) && (isWordRune(rs[j]) || rs[j] == '.') {
				j++
			}
			spans = append(spans, span{string(rs[i:j]), numberLook})
			i = j
		case isWordRune(r):
			j := i
			for j < len(rs) && isWordRune(rs[j]) {
				j++
			}
			w := string(rs[i:j])
			if words[w] {
				spans = append(spans, span{w, keywordLook})
			} else {
				spans = append(spans, plain(w))
			}
			i = j
		default:
			spans = append(spans, plain(string(r)))
			i++
		}
	}
	return spans
}
