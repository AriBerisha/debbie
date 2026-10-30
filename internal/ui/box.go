package ui

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// The input box, like Claude Code's: a rule above and below the line the
// user types on, and a footer with the session's status under it. Line
// mode can't pin it to the bottom of the window, so it is drawn after the
// conversation, which keeps it at the bottom, and replaced by the message
// once the message is sent.

// boxed reports whether the input gets a box: only in a terminal that
// understands escape sequences.
func (c *chat) boxed() bool { return c.term.Interactive && c.term.Decorate }

// rule is a horizontal line one column short of the terminal's width, so
// it never wraps.
func (c *chat) rule() string {
	return c.st.gray(strings.Repeat("─", max(10, c.term.columns()-1)))
}

// drawBox draws the box and leaves the cursor on its input line. For the
// next line of a message being continued, the cursor is where the bottom
// rule was, and the box's lower part moves down a line.
func (c *chat) drawBox(continuing bool) {
	var b strings.Builder
	prompt := c.st.cyan("> ")
	if continuing {
		b.WriteString("\x1b[J") // the old bottom rule and footer
		prompt = c.st.gray(". ")
	} else {
		b.WriteString("\n" + c.rule() + "\n")
	}
	b.WriteString(prompt + "\n" + c.rule() + "\n" + c.footer())
	b.WriteString("\x1b[2A\r\x1b[2C") // back up to the input line, after the prompt
	io.WriteString(c.out, b.String())
	c.out.Flush()
	c.inBox = true
}

// typedRows is how many rows the lines typed into the box take up.
func (c *chat) typedRows() int {
	cols := max(1, c.term.columns())
	rows := 0
	for _, l := range c.typed {
		rows += max(1, (2+utf8.RuneCountInString(l)+cols-1)/cols)
	}
	return rows
}

// closeBox replaces the box with the message as sent, the way Claude Code
// shows it in the conversation; an empty message leaves nothing. The
// cursor is on the line after the last line typed, where the bottom rule
// was.
func (c *chat) closeBox(msg string) {
	if !c.inBox {
		return
	}
	if strings.TrimSpace(msg) == "" {
		c.eraseBox(c.typedRows() + 2) // the box and the blank line above it
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[%dA\r\x1b[J", c.typedRows()+1) // up to the top rule
	for i, l := range strings.Split(msg, "\n") {
		p := "> "
		if i > 0 {
			p = "  "
		}
		b.WriteString(c.st.gray(p) + l + "\n")
	}
	b.WriteString("\n")
	io.WriteString(c.out, b.String())
	c.typed, c.inBox = nil, false
}

// eraseBox clears the screen from up rows above the cursor down.
func (c *chat) eraseBox(up int) {
	fmt.Fprintf(c.out, "\x1b[%dA\r\x1b[J", up)
	c.typed, c.inBox = nil, false
}

// dropBox removes the box and what was typed into it, after Ctrl+C or the
// end of the input. The cursor is on the line being typed. It reports
// whether there was a box.
func (c *chat) dropBox() bool {
	if !c.inBox {
		return false
	}
	// Up past the lines typed, the top rule and the blank line above it.
	c.eraseBox(c.typedRows() + 2)
	return true
}

// footer is the status line under the box: provider, model, mode and
// effort as they are now, with the last turn's context use and speed.
func (c *chat) footer() string {
	parts := []string{c.opts.Status.Provider, c.opts.Status.Model, c.opts.Session.Mode()}
	if e := c.effortShown(); e != "" {
		parts = append(parts, "effort "+e)
	}
	return "  " + c.statusLine(append(parts, c.lastUsage...))
}

// statusLine joins status parts, the third being the mode, in yolo red.
func (c *chat) statusLine(parts []string) string {
	line := c.st.gray(strings.Join(parts[:2], " · ")+" · ") + c.modeText()
	if len(parts) > 3 {
		line += c.st.gray(" · " + strings.Join(parts[3:], " · "))
	}
	return line
}
