//go:build windows

package ui

import (
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleCtrlHandler      = kernel32.NewProc("SetConsoleCtrlHandler")
)

const (
	enableProcessedOutput           = 0x1
	enableVirtualTerminalProcessing = 0x4
)

// consoleOutput returns w, or for a console a writer that hands each write
// to it in one call (see consoleWriter).
func consoleOutput(w io.Writer) io.Writer {
	f, ok := w.(*os.File)
	if !ok || !isTerminal(f) {
		return w
	}
	h := syscall.Handle(f.Fd())
	return &consoleWriter{write: func(units []uint16) (int, error) {
		var n uint32
		err := syscall.WriteConsole(h, &units[0], uint32(len(units)), &n, nil)
		return int(n), err
	}}
}

func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}

// enableANSI turns on virtual-terminal processing, without which a Windows
// console prints escape sequences instead of acting on them.
func enableANSI(f *os.File) (restore func(), ok bool) {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if syscall.GetConsoleMode(h, &mode) != nil {
		return func() {}, false
	}
	if r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableProcessedOutput|enableVirtualTerminalProcessing)); r == 0 {
		return func() {}, false
	}
	return func() { procSetConsoleMode.Call(uintptr(h), uintptr(mode)) }, true
}

// consoleScreenBufferInfo is CONSOLE_SCREEN_BUFFER_INFO.
type consoleScreenBufferInfo struct {
	size, cursor             [2]int16
	attributes               uint16
	left, top, right, bottom int16
	maxWidth, maxHeight      int16
}

// termWidth returns the width of the console window in columns, or 0.
func termWidth(f *os.File) int {
	var info consoleScreenBufferInfo
	if r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0
	}
	return int(info.right-info.left) + 1
}

// trueColor reports whether the terminal shows 24-bit color. The Windows
// console does once virtual-terminal processing is on (Windows 10 1703).
func trueColor() bool { return true }

// enableInterrupts turns Ctrl+C back on in case the program inherited it
// switched off, as children of processes that ignore Ctrl+C do: the chat
// needs it to cancel replies and commands.
func enableInterrupts() { procSetConsoleCtrlHandler.Call(0, 0) }

var procGetCurrentConsoleFontEx = kernel32.NewProc("GetCurrentConsoleFontEx")

// consoleFontInfo is CONSOLE_FONT_INFOEX.
type consoleFontInfo struct {
	size       uint32
	font       uint32
	fontSize   [2]int16
	fontFamily uint32
	fontWeight uint32
	faceName   [32]uint16
}

// consoleFont finds out what the console's font can draw. Windows Terminal
// (WT_SESSION), the terminals that announce themselves in TERM_PROGRAM (VS
// Code, WezTerm) and ConEmu fall back to other fonts. The classic console
// window doesn't, so it depends on its font: Cascadia has the symbols the
// UI uses, Consolas some of them, Lucida Console and the raster fonts few.
func consoleFont(out *os.File) fontSet {
	if os.Getenv("WT_SESSION") != "" || os.Getenv("TERM_PROGRAM") != "" || os.Getenv("ConEmuANSI") == "ON" {
		return fontFull
	}
	info := consoleFontInfo{size: uint32(unsafe.Sizeof(consoleFontInfo{}))}
	if r, _, _ := procGetCurrentConsoleFontEx.Call(out.Fd(), 0, uintptr(unsafe.Pointer(&info))); r == 0 {
		return fontBasic
	}
	switch face := syscall.UTF16ToString(info.faceName[:]); {
	case strings.HasPrefix(face, "Cascadia"):
		return fontFull
	case face == "Consolas":
		return fontConsolas
	}
	return fontBasic
}

// termSize returns the console window's size in columns and rows, or 0s.
func termSize(f *os.File) (cols, rows int) {
	var info consoleScreenBufferInfo
	if r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, 0
	}
	return int(info.right-info.left) + 1, int(info.bottom-info.top) + 1
}

var (
	procSetConsoleScreenBufferSize = kernel32.NewProc("SetConsoleScreenBufferSize")
	procSetConsoleWindowInfo       = kernel32.NewProc("SetConsoleWindowInfo")
)

// fitScreen returns the window's size like termSize, first making the
// screen buffer exactly the window's size when it isn't. The fullscreen UI
// draws on the alternate screen, whose buffer conhost can leave at its old
// size when the window shrinks: the window then shows part of the old
// frame with a horizontal scrollbar, which also costs it a row, and no
// resize event comes, since the buffer didn't change. A buffer more than a
// few rows taller than its window has scrollback, so it is the main
// screen, which is never touched.
func fitScreen(f *os.File) (cols, rows int) {
	var info consoleScreenBufferInfo
	if r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, 0
	}
	cols, rows = int(info.right-info.left)+1, int(info.bottom-info.top)+1
	width, height := int(info.size[0]), int(info.size[1])
	if width == cols && height == rows && info.left == 0 && info.top == 0 || height > rows+3 {
		return cols, rows
	}
	window := [4]int16{0, 0, int16(cols - 1), int16(rows - 1)}
	procSetConsoleWindowInfo.Call(f.Fd(), 1, uintptr(unsafe.Pointer(&window)))
	procSetConsoleScreenBufferSize.Call(f.Fd(), uintptr(uint32(uint16(cols))|uint32(uint16(rows))<<16))
	// Without the scrollbar the window may have room for a row more,
	// which the console gives it with a resize event.
	return termSize(f)
}

var (
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procIsWindowVisible  = user32.NewProc("IsWindowVisible")
)

// consoleWindow reports whether the program has a console window of its
// own, whose right click pastes only with Quick Edit, which the UI turns
// off for the mouse. A pseudo-console's window is hidden: its terminal,
// Windows Terminal or VS Code, pastes as it is set to.
func consoleWindow() bool {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return false
	}
	visible, _, _ := procIsWindowVisible.Call(hwnd)
	return visible != 0
}
