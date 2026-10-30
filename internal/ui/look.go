package ui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Styled text for the fullscreen UI: colors, looks, display widths and
// word wrapping.

// ink is a color: the terminal's default, one of the 16 ANSI colors, or
// RGB.
type ink struct {
	kind uint8 // inkDefault, inkANSI or inkRGB
	n    uint8 // the ANSI color, 0 to 15
	c    rgb
}

const (
	inkDefault = iota
	inkANSI
	inkRGB
)

// The ANSI colors, which follow the user's color scheme.
var (
	inkRed    = ink{kind: inkANSI, n: 1}
	inkGreen  = ink{kind: inkANSI, n: 2}
	inkYellow = ink{kind: inkANSI, n: 3}
	inkBlue   = ink{kind: inkANSI, n: 4}
	inkCyan   = ink{kind: inkANSI, n: 6}
	inkGray   = ink{kind: inkANSI, n: 8}
	inkWhite  = ink{kind: inkANSI, n: 15}
)

func rgbInk(c rgb) ink { return ink{kind: inkRGB, c: c} }

// code returns the SGR parameters that select the color, for the text
// when fg is true, else for the background.
func (k ink) code(fg, trueColor bool) string {
	base := 30
	if !fg {
		base = 40
	}
	switch k.kind {
	case inkANSI:
		if k.n < 8 {
			return strconv.Itoa(base + int(k.n))
		}
		return strconv.Itoa(base + 60 + int(k.n) - 8)
	case inkRGB:
		if trueColor {
			return fmt.Sprintf("%d;2;%d;%d;%d", base+8, k.c.r, k.c.g, k.c.b)
		}
		return fmt.Sprintf("%d;5;%d", base+8, k.c.cube())
	}
	return strconv.Itoa(base + 9)
}

// look is how text is drawn.
type look struct {
	fg, bg                           ink
	bold, italic, underline, reverse bool
}

// sgr returns the escape sequence that switches to l from any look. With
// color off, only the attributes remain.
func (l look) sgr(color, trueColor bool) string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	for _, a := range []struct {
		on   bool
		code string
	}{{l.bold, "1"}, {l.italic, "3"}, {l.underline, "4"}, {l.reverse, "7"}} {
		if a.on {
			b.WriteString(";" + a.code)
		}
	}
	if color && l.fg.kind != inkDefault {
		b.WriteString(";" + l.fg.code(true, trueColor))
	}
	if color && l.bg.kind != inkDefault {
		b.WriteString(";" + l.bg.code(false, trueColor))
	}
	b.WriteByte('m')
	return b.String()
}

// span is text in one look.
type span struct {
	text string
	look look
}

func plain(text string) span          { return span{text: text} }
func inked(text string, k ink) span   { return span{text: text, look: look{fg: k}} }
func styled(text string, l look) span { return span{text: text, look: l} }
func bolded(text string, k ink) span  { return span{text: text, look: look{fg: k, bold: true}} }
func grayed(text string) span         { return inked(text, inkGray) }
func spansWidth(spans []span) (w int) {
	for _, s := range spans {
		w += textWidth(s.text)
	}
	return w
}

// runeWidth is how many columns r takes: none for combining marks and
// other characters that join the one before, two for East Asian wide and
// fullwidth characters and most emoji, else one.
func runeWidth(r rune) int {
	switch {
	case r < 0x300:
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) || r == 0x200b:
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0x303e, r >= 0x3041 && r <= 0x33ff,
		r >= 0x3400 && r <= 0x4dbf, r >= 0x4e00 && r <= 0x9fff, r >= 0xa000 && r <= 0xa4cf,
		r >= 0xac00 && r <= 0xd7a3, r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1f64f,
		r >= 0x1f900 && r <= 0x1f9ff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// textWidth is how many columns s takes.
func textWidth(s string) (w int) {
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// clean makes text from the model, tools or the user safe to draw: tabs
// become spaces, and control characters, which could move the cursor or
// change the terminal's state, become visible ^X marks. Newlines stay.
func clean(s string) string {
	clear := true
	for _, r := range s {
		if r != '\n' && (r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0) || r == utf8.RuneError {
			clear = false
			break
		}
	}
	if clear {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r == '\r':
		case r < 0x20:
			b.WriteString("^" + string(rune('@'+r)))
		case r == 0x7f || r >= 0x80 && r < 0xa0 || r == utf8.RuneError:
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// glyph is one character with its width and look.
type glyph struct {
	r    rune
	w    int
	look look
}

func glyphsOf(spans []span) []glyph {
	var gs []glyph
	for _, s := range spans {
		for _, r := range clean(s.text) {
			if r == '\n' {
				r = ' '
			}
			gs = append(gs, glyph{r, runeWidth(r), s.look})
		}
	}
	return gs
}

func spansOf(gs []glyph) []span {
	var spans []span
	var b strings.Builder
	for i, g := range gs {
		if i > 0 && g.look != gs[i-1].look {
			spans = append(spans, span{b.String(), gs[i-1].look})
			b.Reset()
		}
		b.WriteRune(g.r)
	}
	if len(gs) > 0 {
		spans = append(spans, span{b.String(), gs[len(gs)-1].look})
	}
	return spans
}

// wrapGlyphs breaks a line into rows at spaces where it can; a word longer
// than a row is broken where it must be. The first row is at most first
// columns wide, the others rest. The spaces at a break are dropped.
func wrapGlyphs(gs []glyph, first, rest int) [][]glyph {
	var rows [][]glyph
	for {
		width := max(1, first)
		if len(rows) > 0 {
			width = max(1, rest)
		}
		w, i, space := 0, 0, -1
		for i < len(gs) && w+gs[i].w <= width {
			if gs[i].r == ' ' {
				space = i
			}
			w += gs[i].w
			i++
		}
		if i == len(gs) {
			return append(rows, gs)
		}
		cut := i
		switch {
		case gs[i].r == ' ':
		case space > 0:
			cut = space
		case i == 0:
			cut = 1 // a character wider than the row
		}
		rows = append(rows, trimGlyphs(gs[:cut], false))
		gs = trimGlyphs(gs[cut:], true)
		if len(gs) == 0 {
			return rows
		}
	}
}

// trimGlyphs drops spaces from the end of gs, or from the start when left.
func trimGlyphs(gs []glyph, left bool) []glyph {
	if left {
		for len(gs) > 0 && gs[0].r == ' ' {
			gs = gs[1:]
		}
		return gs
	}
	for len(gs) > 0 && gs[len(gs)-1].r == ' ' {
		gs = gs[:len(gs)-1]
	}
	return gs
}

// wrap lays out text in rows width columns wide: each line of it is word
// wrapped, the first row starts with first and the others with rest,
// markers or indents that count toward the width.
func wrap(spans []span, width int, first, rest []span) [][]span {
	var lines [][]span
	var cur []span
	for _, s := range spans {
		parts := strings.Split(s.text, "\n")
		for i, p := range parts {
			if i > 0 {
				lines = append(lines, cur)
				cur = nil
			}
			if p != "" {
				cur = append(cur, span{p, s.look})
			}
		}
	}
	lines = append(lines, cur)

	var rows [][]span
	for _, line := range lines {
		firstWidth := width - spansWidth(prefixFor(len(rows), first, rest))
		for _, gs := range wrapGlyphs(glyphsOf(line), firstWidth, width-spansWidth(rest)) {
			prefix := prefixFor(len(rows), first, rest)
			rows = append(rows, append(append([]span(nil), prefix...), spansOf(gs)...))
		}
	}
	return rows
}

func prefixFor(row int, first, rest []span) []span {
	if row == 0 {
		return first
	}
	return rest
}

// cutText is text cut to width columns.
func cutText(text string, width int) string { return spansText(cut([]span{plain(text)}, width)) }

// cut shortens spans to at most width columns.
func cut(spans []span, width int) []span {
	var out []span
	for _, s := range spans {
		if width <= 0 {
			break
		}
		if w := textWidth(s.text); w <= width {
			out = append(out, s)
			width -= w
			continue
		}
		var b strings.Builder
		for _, r := range s.text {
			if rw := runeWidth(r); rw <= width {
				b.WriteRune(r)
				width -= rw
			} else {
				width = 0
				break
			}
		}
		out = append(out, span{b.String(), s.look})
	}
	return out
}
