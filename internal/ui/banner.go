package ui

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"debbie/internal/app"
)

// logoBits is the wordmark of debbie.sh, "DEBBIE" in Chintzy CPU by
// Brian Kent (Ænigma Fonts, freeware), as the font itself draws it at 12
// pixels: 8 pixels high, '#' for ink and '.' for paper, the letters a
// column apart since the font sets them touching. The font isn't included.
//
//go:embed logo.txt
var logoBits string

// The logo's gradient, left to right: debbie.sh's accent on dark panels
// to its accent on light ones.
var logoFrom, logoTo = rgb{0x7c, 0xcf, 0xc3}, rgb{0x23, 0x78, 0x6f}

// halves holds the character for a column's top and bottom pixel within a
// row, indexed [top][bottom]. A pixel is one column wide and half a row
// high, so square, and the logo is 4 rows high. Only the full and half
// blocks are used: every console font has them, while the quarter blocks
// (▗, ▙, ▛ and the rest) show up as boxes in the Windows console's fonts.
var halves = [2][2]rune{{' ', '▄'}, {'▀', '█'}}

// logoGap separates the logo from the text beside it.
const logoGap = "   "

type rgb struct{ r, g, b uint8 }

// logoCells turns the bitmap into rows of block characters.
func logoCells() [][]rune {
	rows := strings.Split(strings.TrimSpace(strings.ReplaceAll(logoBits, "\r", "")), "\n")
	ink := func(x, y int) int {
		if y < len(rows) && x < len(rows[y]) && rows[y][x] == '#' {
			return 1
		}
		return 0
	}
	var cells [][]rune
	for y := 0; y < len(rows); y += 2 {
		line := make([]rune, len(rows[0]))
		for x := range line {
			line[x] = halves[ink(x, y)][ink(x, y+1)]
		}
		cells = append(cells, line)
	}
	return cells
}

// logo returns the logo's lines, each the same width, colored with the
// gradient when color is on.
func logo(t Terminal) []string {
	cells := logoCells()
	var lines []string
	for _, row := range cells {
		var b strings.Builder
		for x, r := range row {
			if t.Color && r != ' ' {
				b.WriteString(foreground(logoFrom.mix(logoTo, float64(x)/float64(max(1, len(row)-1))), t.TrueColor))
			}
			b.WriteRune(r)
		}
		if t.Color {
			b.WriteString("\x1b[0m")
		}
		lines = append(lines, b.String())
	}
	return lines
}

// mix returns the color a fraction f of the way from c to d.
func (c rgb) mix(d rgb, f float64) rgb {
	lerp := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*f + 0.5) }
	return rgb{lerp(c.r, d.r), lerp(c.g, d.g), lerp(c.b, d.b)}
}

// foreground sets the text color: exactly in a 24-bit terminal, else the
// nearest color of the 256-color palette's 6×6×6 cube.
func foreground(c rgb, trueColor bool) string {
	if trueColor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", c.cube())
}

// cube returns the nearest color of the 256-color palette's 6×6×6 cube.
func (c rgb) cube() int {
	level := func(v uint8) int {
		steps := []int{0, 95, 135, 175, 215, 255}
		best := 0
		for i, s := range steps {
			if abs(int(v)-s) < abs(int(v)-steps[best]) {
				best = i
			}
		}
		return best
	}
	return 16 + 36*level(c.r) + 6*level(c.g) + level(c.b)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// banner starts the chat like Claude Code does: a cleared screen with the
// logo in the top-left corner, and beside it the program and version, the
// model with its reasoning effort and the provider, and the directory the
// agent works in.
func (c *chat) banner() {
	s := c.opts.Status
	detail := s.Model
	if e := c.effortShown(); e != "" {
		detail += " · effort " + e
	}
	c.Clear()
	c.Banner(detail+" · "+s.Provider, c.opts.Workspace)
}

// Clear wipes the terminal and its scrollback and puts the cursor in the
// top-left corner, as cls does, so the banner starts the screen. It does
// nothing unless the user works in a terminal.
func (c *Console) Clear() {
	if c.term.Interactive && c.term.Decorate {
		io.WriteString(c.out, "\x1b[2J\x1b[3J\x1b[H")
	}
}

// Banner draws the logo with three lines beside it: the program and its
// version, detail (the model, its reasoning effort and the provider), and
// the working directory.
func (c *Console) Banner(detail, dir string) {
	text := []string{
		c.st.bold("Code") + c.st.gray(" v"+app.Version),
		c.st.gray(detail),
		c.st.gray(homeRelative(dir)),
	}
	t := c.term
	art := logo(t)
	width := len(logoCells()[0])
	if !t.Decorate || t.Width > 0 && t.Width < width+len(logoGap)+20 {
		art, width = nil, 0 // no room, or no terminal to draw on
	}
	for i := range max(len(art), len(text)) {
		line := ""
		if i < len(art) {
			line = art[i]
		}
		if i < len(text) {
			switch {
			case i < len(art):
				line += logoGap
			case width > 0:
				line = strings.Repeat(" ", width) + logoGap
			}
			line += text[i]
		}
		c.r.println(line)
	}
	c.r.println("")
}

// homeRelative shortens a path in the user's home directory to ~.
func homeRelative(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || dir == "" {
		return dir
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return dir
	}
	if rel == "." {
		return "~"
	}
	return "~" + string(filepath.Separator) + rel
}
