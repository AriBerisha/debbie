package ui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// editor is the text typed into the fullscreen UI's input box: its
// characters, the cursor among them, and the messages sent before, for Up
// and Down.
type editor struct {
	text   []rune
	cursor int // where the next character goes, 0 to len(text)
	// anchor is the end of the selection that stays put while the cursor
	// moves the other one, as Shift with a movement key or a drag of the
	// mouse selects; -1 when nothing is selected.
	anchor int

	history []string // oldest first
	browse  int      // the history entry shown; len(history) when none
	draft   string   // what was typed before browsing
	cleared string   // what Ctrl+C cleared, which ↑ brings back
	column  int      // the column Up and Down keep to; -1 for the cursor's

	chips    map[rune]chip // pastes shown as chips, by the character standing for each
	nextChip rune          // the character the next chip gets
}

// A paste too long for the input box shows there as a chip, as in Claude
// Code: "[Pasted text #1 +52 lines]". The chip is one character of the
// text, from Unicode's supplementary private use area B, which stands for
// the paste: Backspace deletes it whole, the cursor steps over it, and the
// message sent has the pasted text in its place (expand). Characters of
// that area in what is typed or pasted are dropped, so none is taken for
// a chip.
const (
	chipFirst rune = 0x100000
	chipLast  rune = 0x10fffd
	// A paste becomes a chip when it has more lines than chipLines or more
	// characters than chipChars.
	chipLines = 3
	chipChars = 800
)

// chip is a paste the input box shows as its label.
type chip struct {
	text, label string
	n           int // its number in the message, from 1
}

func newEditor(history []string) *editor {
	return &editor{history: history, browse: len(history), anchor: -1, column: -1, chips: map[rune]chip{}, nextChip: chipFirst}
}

// The selection.

// selection returns the selected range, from inclusive to exclusive; ok
// is false when nothing is selected.
func (e *editor) selection() (from, to int, ok bool) {
	if e.anchor < 0 || e.anchor == e.cursor {
		return 0, 0, false
	}
	return min(e.anchor, e.cursor), max(e.anchor, e.cursor), true
}

func (e *editor) hasSelection() bool {
	_, _, ok := e.selection()
	return ok
}

// selectedText is the selected text, a chip's paste in full.
func (e *editor) selectedText() string {
	from, to, ok := e.selection()
	if !ok {
		return ""
	}
	return e.expand(string(e.text[from:to]))
}

func (e *editor) deselect() { e.anchor = -1 }

// selectAll selects the whole text, the cursor at its end.
func (e *editor) selectAll() {
	e.anchor, e.cursor, e.column = 0, len(e.text), -1
}

// selectWordAt selects the word around position i, a path or a URL being
// one word; it reports false when there is none there.
func (e *editor) selectWordAt(i int) bool {
	if i < 0 || i >= len(e.text) || !wordChar(e.text[i]) {
		return false
	}
	from, to := i, i+1
	for from > 0 && wordChar(e.text[from-1]) {
		from--
	}
	for to < len(e.text) && wordChar(e.text[to]) {
		to++
	}
	// A trailing period or comma ends a sentence, not the word.
	for to-1 > from && strings.ContainsRune(".,:!?", e.text[to-1]) {
		to--
	}
	e.anchor, e.cursor, e.column = from, to, -1
	return true
}

// extend starts a selection at the cursor, if none is under way, before
// the cursor moves with Shift held.
func (e *editor) extend() {
	if e.anchor < 0 {
		e.anchor = e.cursor
	}
}

// deleteSelection removes the selected text, as typing over it or
// Backspace does; it reports whether there was one.
func (e *editor) deleteSelection() bool {
	from, to, ok := e.selection()
	e.anchor = -1
	if !ok {
		return false
	}
	e.text = append(e.text[:from], e.text[to:]...)
	e.cursor, e.column = from, -1
	return true
}

// clear empties the editor, as Ctrl+C does, keeping the text for ↑ to
// bring back.
func (e *editor) clear() {
	if !e.empty() {
		e.cleared = e.value()
	}
	e.reset()
}

// paste inserts pasted text at the cursor: as it is, or as a chip when it
// is long. Its line endings become newlines; its tabs stay in a chip.
func (e *editor) paste(s string) {
	e.deleteSelection()
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	lines := strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
	chars := utf8.RuneCountInString(s)
	if lines <= chipLines && chars <= chipChars || e.nextChip > chipLast {
		e.insert(s)
		return
	}
	var b strings.Builder
	for _, r := range s {
		if (r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f) && !inChipArea(r) {
			b.WriteRune(r)
		}
	}
	n := 1 // after the chips in the message
	for _, r := range e.text {
		if c, ok := e.chips[r]; ok && c.n >= n {
			n = c.n + 1
		}
	}
	label := fmt.Sprintf("[Pasted text #%d +%d lines]", n, lines)
	if lines == 1 {
		label = fmt.Sprintf("[Pasted text #%d +%d chars]", n, chars)
	}
	r := e.nextChip
	e.nextChip++
	e.chips[r] = chip{text: b.String(), label: label, n: n}
	e.text = append(e.text[:e.cursor], append([]rune{r}, e.text[e.cursor:]...)...)
	e.cursor++
	e.column = -1
}

func inChipArea(r rune) bool { return r >= chipFirst && r <= chipLast }

// expand returns text, as value gives it, with each chip's paste in its
// place: what is sent.
func (e *editor) expand(text string) string {
	return e.replaceChips(text, func(c chip) string { return c.text })
}

// display returns text with each chip's label in its place: what the
// conversation shows.
func (e *editor) display(text string) string {
	return e.replaceChips(text, func(c chip) string { return c.label })
}

// typed returns text with the chips left out, as spaces: what the user
// typed, in which mentions are looked for.
func (e *editor) typed(text string) string {
	return e.replaceChips(text, func(chip) string { return " " })
}

func (e *editor) replaceChips(text string, with func(chip) string) string {
	if !strings.ContainsFunc(text, inChipArea) {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		if c, ok := e.chips[r]; ok {
			b.WriteString(with(c))
		} else if !inChipArea(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// savedHistory is the history to keep on disk: the chips' pastes in full,
// since the next session has no chips.
func (e *editor) savedHistory() []string {
	out := make([]string, len(e.history))
	for i, h := range e.history {
		out[i] = e.expand(h)
	}
	return out
}

func (e *editor) value() string { return string(e.text) }
func (e *editor) empty() bool   { return len(e.text) == 0 }

// set replaces the text and puts the cursor at its end.
func (e *editor) set(s string) {
	e.text = []rune(s)
	e.cursor, e.anchor, e.column = len(e.text), -1, -1
}

// reset empties the editor and stops browsing the history.
func (e *editor) reset() {
	e.set("")
	e.browse, e.draft = len(e.history), ""
}

// insert types s at the cursor. Carriage returns become newlines and tabs
// four spaces; other control characters are dropped, and so are the
// characters that stand for chips.
func (e *editor) insert(s string) {
	e.deleteSelection()
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", "    ").Replace(s)
	var rs []rune
	for _, r := range s {
		if (r == '\n' || r >= 0x20 && r != 0x7f) && !inChipArea(r) {
			rs = append(rs, r)
		}
	}
	e.text = append(e.text[:e.cursor], append(rs, e.text[e.cursor:]...)...)
	e.cursor += len(rs)
	e.column = -1
}

func (e *editor) backspace() {
	if e.deleteSelection() {
		return
	}
	if e.cursor > 0 {
		e.text = append(e.text[:e.cursor-1], e.text[e.cursor:]...)
		e.cursor--
	}
	e.column = -1
}

func (e *editor) del() {
	if e.deleteSelection() {
		return
	}
	if e.cursor < len(e.text) {
		e.text = append(e.text[:e.cursor], e.text[e.cursor+1:]...)
	}
	e.column = -1
}

func (e *editor) left()  { e.cursor, e.column = max(0, e.cursor-1), -1 }
func (e *editor) right() { e.cursor, e.column = min(len(e.text), e.cursor+1), -1 }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// wordLeft moves to the start of the word before the cursor.
func (e *editor) wordLeft() {
	i := e.cursor
	for i > 0 && !isWordRune(e.text[i-1]) {
		i--
	}
	for i > 0 && isWordRune(e.text[i-1]) {
		i--
	}
	e.cursor, e.column = i, -1
}

// wordRight moves to the end of the word after the cursor.
func (e *editor) wordRight() {
	i := e.cursor
	for i < len(e.text) && !isWordRune(e.text[i]) {
		i++
	}
	for i < len(e.text) && isWordRune(e.text[i]) {
		i++
	}
	e.cursor, e.column = i, -1
}

// lineStart and lineEnd are the bounds of the line the cursor is on.
func (e *editor) lineStart() int {
	i := e.cursor
	for i > 0 && e.text[i-1] != '\n' {
		i--
	}
	return i
}

func (e *editor) lineEnd() int {
	i := e.cursor
	for i < len(e.text) && e.text[i] != '\n' {
		i++
	}
	return i
}

func (e *editor) home() { e.cursor, e.column = e.lineStart(), -1 }
func (e *editor) end()  { e.cursor, e.column = e.lineEnd(), -1 }

// killStart deletes from the start of the line to the cursor (Ctrl+U),
// killEnd from the cursor to the end of the line (Ctrl+K), and killWord the
// word before the cursor (Ctrl+W).
func (e *editor) killStart() {
	e.anchor = -1
	start := e.lineStart()
	e.text = append(e.text[:start], e.text[e.cursor:]...)
	e.cursor, e.column = start, -1
}

func (e *editor) killEnd() {
	e.anchor = -1
	e.text = append(e.text[:e.cursor], e.text[e.lineEnd():]...)
	e.column = -1
}

func (e *editor) killWord() {
	e.anchor = -1
	end := e.cursor
	e.wordLeft()
	e.text = append(e.text[:e.cursor], e.text[end:]...)
}

// layout wraps the text into rows width columns wide and finds the cursor:
// rows holds each row's runes, and pos the row and column of every
// position in the text, the end included.
func (e *editor) layout(width int) (rows [][]rune, pos [][2]int) {
	rows, pos, _, _ = e.layoutChips(width)
	return rows, pos
}

// layoutChips is layout that also marks the characters of chips' labels,
// for drawing them apart, and says which position of the text each
// character drawn stands for, so a selection can be drawn. A chip is its
// label, which starts a row of its own when the row has no room for it.
func (e *editor) layoutChips(width int) (rows [][]rune, pos [][2]int, chipped [][]bool, owner [][]int) {
	width = max(1, width)
	rows, chipped, owner = [][]rune{nil}, [][]bool{nil}, [][]int{nil}
	col := 0
	newRow := func() {
		rows, chipped, owner = append(rows, nil), append(chipped, nil), append(owner, nil)
		col = 0
	}
	put := func(r rune, w int, ofChip bool, at int) {
		rows[len(rows)-1] = append(rows[len(rows)-1], r)
		chipped[len(chipped)-1] = append(chipped[len(chipped)-1], ofChip)
		owner[len(owner)-1] = append(owner[len(owner)-1], at)
		col += w
	}
	for i, r := range e.text {
		if c, ok := e.chips[r]; ok {
			if col > 0 && col+textWidth(c.label) > width {
				newRow()
			}
			pos = append(pos, [2]int{len(rows) - 1, col})
			for _, l := range c.label { // a label wider than a row wraps, as text does
				w := runeWidth(l)
				if col > 0 && col+w > width {
					newRow()
				}
				put(l, w, true, i)
			}
			continue
		}
		w := runeWidth(r)
		if r != '\n' && col+w > width {
			newRow()
		}
		pos = append(pos, [2]int{len(rows) - 1, col})
		if r == '\n' {
			newRow()
			continue
		}
		put(r, w, false, i)
	}
	if col >= width { // the cursor at the end of a full row starts the next
		newRow()
	}
	pos = append(pos, [2]int{len(rows) - 1, col})
	return rows, pos, chipped, owner
}

// positionAt returns the position of the text drawn at a row and column
// of a layout: the last one on the row that starts at or before the
// column, or the row's first; -1 when the row has none.
func (e *editor) positionAt(width, row, col int) int {
	_, pos := e.layout(width)
	best := -1
	for i, p := range pos {
		if p[0] == row && (best < 0 || p[1] <= col) {
			best = i
		}
	}
	return best
}

// cursorAt returns the row and column of the cursor in a layout.
func (e *editor) cursorAt(width int) (row, col int) {
	_, pos := e.layout(width)
	p := pos[e.cursor]
	return p[0], p[1]
}

// vertical moves the cursor one row up (dir -1) or down (dir 1) in a
// layout width columns wide, keeping to its column. It reports false when
// there is no row there.
func (e *editor) vertical(dir, width int) bool {
	rows, pos := e.layout(width)
	cur := pos[e.cursor]
	target := cur[0] + dir
	if target < 0 || target >= len(rows) {
		return false
	}
	if e.column < 0 {
		e.column = cur[1]
	}
	best := -1
	for i, p := range pos {
		if p[0] == target && p[1] <= e.column {
			best = i
		}
	}
	if best < 0 { // only positions past the column: the row's first
		for i, p := range pos {
			if p[0] == target {
				best = i
				break
			}
		}
	}
	e.cursor = best
	return true
}

// up moves the cursor a row up or, from the first row, to what Ctrl+C
// cleared, then to the previous message in the history.
func (e *editor) up(width int) {
	if e.vertical(-1, width) {
		return
	}
	if e.cleared != "" && e.browse == len(e.history) && e.empty() {
		e.set(e.cleared)
		e.cleared = ""
		return
	}
	if e.browse == 0 {
		return
	}
	if e.browse == len(e.history) {
		e.draft = e.value()
	}
	e.browse--
	e.set(e.history[e.browse])
}

// down moves the cursor a row down or, from the last row, to the next
// message in the history, and past the newest back to what was typed.
func (e *editor) down(width int) {
	if e.vertical(1, width) {
		return
	}
	if e.browse >= len(e.history) {
		return
	}
	e.browse++
	if e.browse == len(e.history) {
		e.set(e.draft)
		return
	}
	e.set(e.history[e.browse])
}

// remember adds a sent message to the history, unless it repeats the last.
func (e *editor) remember(msg string) {
	if strings.TrimSpace(msg) != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != msg) {
		e.history = append(e.history, msg)
	}
	e.browse = len(e.history)
}
