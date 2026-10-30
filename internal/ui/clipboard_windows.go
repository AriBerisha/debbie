package ui

import (
	"errors"
	"io"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// copyText puts text on the Windows clipboard, through the clipboard API
// itself; no program is started. It returns where the text went.
func copyText(text string, _ []string, _ io.Writer) (string, error) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\x00", ""), "\n", "\r\n")
	u, err := syscall.UTF16FromString(text)
	if err != nil {
		return "", err
	}
	// Another program may hold the clipboard for a moment.
	opened := false
	for range 10 {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !opened {
		return "", errors.New("another program is using the clipboard")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	size := uintptr(len(u)) * 2
	h, _, err := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return "", err
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return "", err
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), size)
	procGlobalUnlock.Call(h)
	if r, _, err := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h) // the clipboard owns it only once accepted
		return "", err
	}
	return "the clipboard", nil
}

var (
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
)

// pasteText reads the text on the Windows clipboard, through the
// clipboard API itself.
func pasteText(_ []string) (string, error) {
	if r, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return "", errors.New("the clipboard holds no text")
	}
	opened := false
	for range 10 {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !opened {
		return "", errors.New("another program is using the clipboard")
	}
	defer procCloseClipboard.Call()
	h, _, err := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", err
	}
	size, _, _ := procGlobalSize.Call(h)
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		return "", err
	}
	defer procGlobalUnlock.Call(h)
	if size < 2 {
		return "", nil
	}
	u := make([]uint16, size/2)
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&u[0])), p, size)
	return strings.ReplaceAll(syscall.UTF16ToString(u), "\r\n", "\n"), nil
}
