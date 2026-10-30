//go:build !windows

package ui

import (
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// consoleOutput returns w: a terminal takes a write of any length.
func consoleOutput(w io.Writer) io.Writer { return w }

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// enableANSI does nothing: Unix terminals understand ANSI escape sequences.
func enableANSI(*os.File) (restore func(), ok bool) { return func() {}, true }

// termWidth returns the width of the terminal in columns, or 0.
func termWidth(f *os.File) int {
	var ws struct{ rows, cols, x, y uint16 }
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return 0
	}
	return int(ws.cols)
}

// trueColor reports whether the terminal shows 24-bit color, which
// terminals announce in COLORTERM.
func trueColor() bool {
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return true
	}
	return false
}

// enableInterrupts is for Windows, where Ctrl+C can be inherited switched
// off.
func enableInterrupts() {}

// consoleFont is for the classic Windows console; Unix terminals fall back
// to other fonts for symbols theirs lacks.
func consoleFont(*os.File) fontSet { return fontFull }

// fitScreen returns the terminal's size: a terminal has no buffer wider
// than its window to fit.
func fitScreen(f *os.File) (cols, rows int) { return termSize(f) }

// termSize returns the terminal's size in columns and rows, or 0s.
func termSize(f *os.File) (cols, rows int) {
	var ws struct{ rows, cols, x, y uint16 }
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		return 0, 0
	}
	return int(ws.cols), int(ws.rows)
}

// consoleWindow is the Windows console window: there is none here, and
// the terminal pastes.
func consoleWindow() bool { return false }
