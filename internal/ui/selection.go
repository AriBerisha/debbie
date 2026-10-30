package ui

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Selecting text with the mouse, like Claude Code's fullscreen mode, since
// capturing the mouse takes the terminal's own selection away: a drag over
// the conversation selects, a double-click selects a word (a path or a URL
// as one), a triple-click a line, and the text goes to the clipboard when
// the button is released. Ctrl+C copies it again.

// cellPos is a cell of the conversation: its row in the whole conversation,
// so a selection stays on its text while the view scrolls, and its column.
type cellPos struct{ row, col int }

func (a cellPos) before(b cellPos) bool { return a.row < b.row || a.row == b.row && a.col < b.col }

type selection struct {
	anchor, head cellPos
	active       bool // something is selected
	dragging     bool // the button went down in the conversation and is still down
}

// bounds returns the selection's first and last cells, in order.
func (s *selection) bounds() (cellPos, cellPos) {
	if s.head.before(s.anchor) {
		return s.head, s.anchor
	}
	return s.anchor, s.head
}

// columns returns the columns from..to (exclusive) selected in a row.
func (s *selection) columns(row, width int) (from, to int, ok bool) {
	if !s.active {
		return 0, 0, false
	}
	a, b := s.bounds()
	if row < a.row || row > b.row {
		return 0, 0, false
	}
	from, to = 0, width
	if row == a.row {
		from = a.col
	}
	if row == b.row {
		to = b.col + 1
	}
	return from, to, from < to
}

// reverseCells draws the cells from..to of a row in reverse video.
func reverseCells(spans []span, from, to int) []span {
	var out []span
	col := 0
	for _, s := range spans {
		var cur strings.Builder
		curSel := false
		emit := func() {
			if cur.Len() > 0 {
				l := s.look
				if curSel {
					l.reverse = !l.reverse
				}
				out = append(out, span{cur.String(), l})
				cur.Reset()
			}
		}
		for _, r := range s.text {
			w := runeWidth(r)
			sel := col < to && col+w > from
			if sel != curSel {
				emit()
				curSel = sel
			}
			cur.WriteRune(r)
			col += w
		}
		emit()
	}
	if col < to { // a selection that goes on past the row's text
		out = append(out, span{strings.Repeat(" ", to-max(col, from)), look{reverse: true}})
	}
	return out
}

// cutColumns returns the characters of s in the columns from..to.
func cutColumns(s string, from, to int) string {
	var b strings.Builder
	col := 0
	for _, r := range s {
		w := runeWidth(r)
		if col >= from && col+w <= to {
			b.WriteRune(r)
		}
		col += w
	}
	return b.String()
}

// selectedText is the text selected, a line per row, without the spaces at
// the end of each, and without the margin the transcript indents them by:
// the indentation the lines share goes, what they have beyond it stays.
func (f *fullscreen) selectedText() string {
	a, b := f.sel.bounds()
	var lines []string
	for r := a.row; r <= b.row && r < len(f.conv); r++ {
		from, to, _ := f.sel.columns(r, f.width)
		lines = append(lines, strings.TrimRight(cutColumns(spansText(f.conv[r]), from, to), " "))
	}
	first := 1 // a selection that starts within a line keeps that line as it is
	if a.col == 0 {
		first = 0
	}
	margin := -1
	for _, l := range lines[min(first, len(lines)):] {
		if l != "" {
			if n := len(l) - len(strings.TrimLeft(l, " ")); margin < 0 || n < margin {
				margin = n
			}
		}
	}
	for i := first; i < len(lines) && margin > 0; i++ {
		lines[i] = lines[i][min(margin, len(lines[i])):]
	}
	return strings.Join(lines, "\n")
}

// copySelection puts the selected text on the clipboard and says so.
func (f *fullscreen) copySelection() {
	text := f.selectedText()
	if strings.TrimSpace(text) == "" {
		return
	}
	via, err := f.clipboard(text)
	if err != nil {
		f.flash("couldn't copy: " + err.Error())
		return
	}
	n := len([]rune(text))
	f.flash(fmt.Sprintf("copied %d character%s to %s", n, pluralS(n), via))
}

// copyInputSelection copies the text selected in the input box, a chip's
// paste in full; the text and the selection stay.
func (f *fullscreen) copyInputSelection() {
	f.copyText(f.ed.selectedText())
}

// copyKey copies on Ctrl+Shift+C or Ctrl+Insert: the text selected in the
// conversation, else the text selected in the input box, else all of it.
func (f *fullscreen) copyKey() {
	switch {
	case f.sel.active:
		f.copySelection()
	case f.ed.hasSelection():
		f.copyInputSelection()
	case !f.ed.empty():
		f.copyText(f.ed.expand(f.ed.value()))
	default:
		f.flash("nothing to copy: select text with the mouse first")
	}
}

// copyText puts text on the clipboard and says so in the status line.
func (f *fullscreen) copyText(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	via, err := f.clipboard(text)
	if err != nil {
		f.flash("couldn't copy: " + err.Error())
		return
	}
	n := len([]rune(text))
	f.flash(fmt.Sprintf("copied %d character%s to %s", n, pluralS(n), via))
}

// clipboard puts text on the clipboard and says how it got there.
func (f *fullscreen) clipboard(text string) (via string, err error) {
	if f.copier != nil {
		return f.copier(text)
	}
	var env []string
	if f.opts.ChildEnv != nil {
		env = f.opts.ChildEnv()
	}
	return copyText(text, env, f.out)
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// wordChar reports whether r is part of a word for a double-click, as in
// iTerm2, so a path or a URL is one word.
func wordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(`/\-+~_.:@#%&=?!$*`, r)
}

// selectWord selects the word at a cell, if there is one, and copies it.
func (f *fullscreen) selectWord(p cellPos) {
	if p.row >= len(f.conv) {
		return
	}
	var cells []rune // a rune per column; wide characters fill two
	for _, r := range spansText(f.conv[p.row]) {
		cells = append(cells, r)
		if runeWidth(r) == 2 {
			cells = append(cells, 0)
		}
	}
	if p.col >= len(cells) || !wordChar(cells[p.col]) {
		return
	}
	from, to := p.col, p.col
	for from > 0 && (cells[from-1] == 0 || wordChar(cells[from-1])) {
		from--
	}
	for to+1 < len(cells) && (cells[to+1] == 0 || wordChar(cells[to+1])) {
		to++
	}
	// A trailing period or comma ends a sentence, not the word.
	for to > from && strings.ContainsRune(".,:!?", cells[to]) {
		to--
	}
	f.sel = selection{anchor: cellPos{p.row, from}, head: cellPos{p.row, to}, active: true}
	f.copySelection()
}

// selectLine selects a whole row, from its first character, and copies it.
func (f *fullscreen) selectLine(row int) {
	if row >= len(f.conv) {
		return
	}
	text := spansText(f.conv[row])
	start := textWidth(text) - textWidth(strings.TrimLeft(text, " "))
	end := textWidth(strings.TrimRight(text, " ")) - 1
	if end < start {
		return
	}
	f.sel = selection{anchor: cellPos{row, start}, head: cellPos{row, end}, active: true}
	f.copySelection()
}

// multiClick counts presses at the same cell in quick succession: 1, 2 or
// 3, then 1 again.
func (f *fullscreen) multiClick(x, y int) int {
	now := time.Now()
	if now.Sub(f.lastPress) < 400*time.Millisecond && f.pressAt == [2]int{x, y} {
		f.clicks = f.clicks%3 + 1
	} else {
		f.clicks = 1
	}
	f.lastPress, f.pressAt = now, [2]int{x, y}
	return f.clicks
}

// pasteClipboard pastes the clipboard's text into the input box, for
// Ctrl+V and, on Windows, a right click: what the terminal leaves to the
// program. A long text becomes a chip, as any paste does.
func (f *fullscreen) pasteClipboard() {
	read := f.paster
	if read == nil {
		read = func() (string, error) {
			var env []string
			if f.opts.ChildEnv != nil {
				env = f.opts.ChildEnv()
			}
			return pasteText(env)
		}
	}
	switch text, err := read(); {
	case err != nil:
		f.flash("Couldn't paste: " + err.Error())
	case text != "":
		if pt, ok := f.panel.(pasteTaker); ok {
			pt.paste(f, text)
			return
		}
		f.ed.paste(text)
		f.completion = -1
	}
}
