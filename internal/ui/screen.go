package ui

import (
	"fmt"
	"io"
	"strings"
)

// screen draws the fullscreen UI's frames. It rewrites only the rows that
// changed since the last frame, all in one write with the cursor hidden
// and, where the terminal supports it, synchronized output on, so nothing
// flickers.
type screen struct {
	out              io.Writer
	color, trueColor bool
	width, height    int
	shown            []string // each row as last written; nil before the first frame
	cursor           string   // the cursor's position and visibility as last set
}

// frameRow is one row of a frame: spans, cut to the width, and the look of
// the rest of the row, so a background band reaches the edge.
type frameRow struct {
	spans []span
	fill  look
}

// frame is what the screen shows.
type frame struct {
	rows             []frameRow
	cursorX, cursorY int
	cursor           bool // show the cursor there
}

// Escape sequences that enter and leave the fullscreen UI: the alternate
// screen (no scrollback to scroll past), no line wrap (a full last row
// mustn't scroll the screen), mouse reporting with SGR coordinates, and
// bracketed paste.
const (
	enterFullscreen = "\x1b[?1049h\x1b[?7l\x1b[?25l\x1b[?1000h\x1b[?1002h\x1b[?1006h\x1b[?2004h\x1b[H\x1b[2J"
	leaveFullscreen = "\x1b[?2004l\x1b[?1006l\x1b[?1002l\x1b[?1000l\x1b[0m\x1b[?7h\x1b[?25h\x1b[?1049l"
)

// resize makes the next frame a full redraw at the new size.
func (s *screen) resize(width, height int) {
	if width != s.width || height != s.height {
		s.width, s.height, s.shown = width, height, nil
	}
}

// redraw makes the next frame a full redraw, to repair the screen.
func (s *screen) redraw() { s.shown = nil }

// encode writes a row as escape sequences and text, exactly the width of
// the screen.
func (s *screen) encode(r frameRow) string {
	var b strings.Builder
	w := 0
	for _, sp := range cut(r.spans, s.width) {
		if sp.text == "" {
			continue
		}
		b.WriteString(sp.look.sgr(s.color, s.trueColor))
		b.WriteString(sp.text)
		w += textWidth(sp.text)
	}
	if w < s.width {
		b.WriteString(r.fill.sgr(s.color, s.trueColor))
		b.WriteString(strings.Repeat(" ", s.width-w))
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// draw shows a frame.
func (s *screen) draw(f frame) {
	var b strings.Builder
	if s.shown == nil {
		s.shown = make([]string, s.height)
		s.cursor = ""
		b.WriteString("\x1b[0m\x1b[2J")
	}
	for y := range s.height {
		var row frameRow
		if y < len(f.rows) {
			row = f.rows[y]
		}
		line := s.encode(row)
		if s.shown[y] == line {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s", y+1, line)
		s.shown[y] = line
	}
	cursor := "\x1b[?25l"
	if f.cursor {
		cursor = fmt.Sprintf("\x1b[%d;%dH\x1b[?25h", f.cursorY+1, f.cursorX+1)
	}
	if b.Len() == 0 && cursor == s.cursor {
		return
	}
	s.cursor = cursor
	data := "\x1b[?2026h\x1b[?25l" + b.String() + cursor + "\x1b[?2026l"
	if w, ok := s.out.(completeWriter); ok {
		w.WriteComplete([]byte(data))
	} else {
		io.WriteString(s.out, data)
	}
}

// completeWriter takes output that is complete in itself and writes it all
// at once, as config.Writer does: a frame's end mustn't be held back,
// waiting for the next frame, because it might start a secret.
type completeWriter interface {
	WriteComplete(p []byte) (int, error)
}
