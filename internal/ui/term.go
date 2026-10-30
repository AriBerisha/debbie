// Package ui is the terminal interface: the fullscreen UI (SPEC §14, phase
// B) in a terminal, and line mode (phase A) when the input or output isn't
// one, or when the user asks for it.
package ui

import "os"

// Terminal describes the console the program runs in.
type Terminal struct {
	// Interactive is true when stdin is a terminal and the user types at a
	// prompt.
	Interactive bool
	// Decorate is true when stdout is a terminal that understands ANSI
	// escape sequences: a spinner, borders around code.
	Decorate bool
	// Color is true when Decorate is and NO_COLOR isn't set.
	Color bool
	// TrueColor is true when the terminal shows 24-bit color; otherwise
	// colors are approximated with the 256-color palette.
	TrueColor bool
	// Width is the terminal's width in columns at the start, 0 when unknown.
	Width int
	// Font is what the terminal's font can draw. The classic Windows console
	// doesn't fall back to other fonts, and shows boxes for symbols its font
	// lacks.
	Font fontSet
	in   *os.File // the terminal's input, for single key presses
	out  *os.File // the terminal, to ask for its width as it is now
}

// fontSet is a set of symbols a terminal's font has.
type fontSet uint8

const (
	fontFull     fontSet = iota // a terminal that falls back to other fonts, or Cascadia
	fontConsolas                // ● and ━ but not ✓, ❯ or ╭
	fontBasic                   // Lucida Console and the like: only the old console set
)

// symbols are the symbols the UI marks things with.
type symbols struct {
	check   string // a check that passed
	pointer string // the suggested or selected choice
	bullet  string // a reply or a tool call
	cross   string // a call that failed
	elbow   string // under a tool call, its result
	goal    string // a goal is set
	forward string // edits, or everything, go ahead without asking
	pause   string // plan mode: nothing changes
	edit    string // editor mode: edits, no commands
	corners [4]string
}

// sym returns the symbols the terminal's font has.
func (t Terminal) sym() symbols {
	s := symbols{check: "✓", pointer: "❯", bullet: "●", cross: "×", elbow: "└", goal: "◎",
		forward: "⏵⏵", pause: "⏸", edit: "✎", corners: [4]string{"╭", "╮", "╰", "╯"}}
	switch t.Font {
	case fontConsolas:
		s.check, s.pointer, s.goal, s.forward, s.pause, s.edit = "√", ">", "○", "►►", "▌▌", "¶"
	case fontBasic:
		s.check, s.pointer, s.bullet, s.goal, s.forward, s.pause, s.edit = "√", ">", "•", "○", "►►", "▌▌", "¶"
		s.corners = [4]string{"┌", "┐", "└", "┘"}
	}
	return s
}

// columns returns the terminal's width now, or else the width it had at the
// start, or else 80.
func (t Terminal) columns() int {
	if t.out != nil {
		if w := termWidth(t.out); w > 0 {
			return w
		}
	}
	if t.Width > 0 {
		return t.Width
	}
	return 80
}

// Setup inspects stdin and stdout and, on Windows, turns on the console's
// ANSI support. The returned function undoes that.
func Setup(stdin, stdout *os.File) (Terminal, func()) {
	t := Terminal{Interactive: isTerminal(stdin)}
	if t.Interactive {
		enableInterrupts()
		t.in = stdin
	}
	restore := func() {}
	if isTerminal(stdout) && os.Getenv("TERM") != "dumb" {
		var ok bool
		restore, ok = enableANSI(stdout)
		t.Decorate = ok
		t.Color = ok && os.Getenv("NO_COLOR") == ""
		t.TrueColor = t.Color && trueColor()
		t.Width = termWidth(stdout)
		t.Font = consoleFont(stdout)
		t.out = stdout
	}
	return t, restore
}

// style paints text with ANSI colors when they are on.
type style struct{ color bool }

func (s style) paint(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) gray(text string) string   { return s.paint("90", text) }
func (s style) cyan(text string) string   { return s.paint("36", text) }
func (s style) red(text string) string    { return s.paint("31", text) }
func (s style) green(text string) string  { return s.paint("32", text) }
func (s style) bold(text string) string   { return s.paint("1", text) }
func (s style) yellow(text string) string { return s.paint("33", text) }

// cursor marks where typing goes, where the terminal's own cursor is
// hidden: a block, or _ without color.
func (s style) cursor() string {
	if !s.color {
		return "_"
	}
	return "\x1b[7m \x1b[27m"
}
