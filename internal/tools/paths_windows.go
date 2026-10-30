//go:build windows

package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathNameByHandleW = kernel32.NewProc("GetFinalPathNameByHandleW")
)

const fileFlagBackupSemantics = 0x02000000 // needed to open directories

// resolveExisting asks Windows for the final path of an existing file or
// directory. Unlike filepath.EvalSymlinks, which no longer follows junctions
// since Go 1.23, this follows symbolic links and junctions alike, and it
// also expands 8.3 short names.
func resolveExisting(p string) (string, error) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := syscall.CreateFile(name, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, fileFlagBackupSemantics, 0)
	if err != nil {
		return "", err // includes links whose target is missing
	}
	defer syscall.CloseHandle(h)
	buf := make([]uint16, 512)
	for {
		n, _, callErr := procGetFinalPathNameByHandleW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if n == 0 {
			return "", callErr
		}
		if int(n) < len(buf) {
			s := syscall.UTF16ToString(buf[:n])
			if rest, ok := strings.CutPrefix(s, `\\?\UNC\`); ok {
				return `\\` + rest, nil
			}
			return strings.TrimPrefix(s, `\\?\`), nil
		}
		buf = make([]uint16, n)
	}
}

// reservedNames are DOS device names, which refer to devices in any
// directory and with any extension.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// checkPathSyntax rejects Windows path forms that don't name an ordinary
// file the way they appear to: device namespaces, paths relative to another
// drive's current directory, alternate data streams, device names, and
// names ending in a dot or space, which Windows silently trims.
func checkPathSyntax(p string) error {
	switch {
	case strings.ContainsRune(p, 0):
		return errors.New("the path contains a NUL character")
	case strings.HasPrefix(p, `\\?\`), strings.HasPrefix(p, `\\.\`), strings.HasPrefix(p, `//?/`), strings.HasPrefix(p, `//./`):
		return errors.New(`device paths (\\?\ and \\.\) are not supported`)
	}
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	if len(vol) == 2 && vol[1] == ':' && !strings.HasPrefix(rest, `\`) && !strings.HasPrefix(rest, "/") && rest != "" {
		return fmt.Errorf("%q is relative to the current directory of drive %s; use %s\\%s or a path relative to the workspace", p, vol, vol, rest)
	}
	if vol == "" && (strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/")) {
		return fmt.Errorf("%q has no drive letter; use a full path such as C:%s or a path relative to the workspace", p, p)
	}
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		if part == "." || part == ".." {
			continue
		}
		if strings.ContainsRune(part, ':') {
			return fmt.Errorf("%q names an alternate data stream (\":\" in a file name)", p)
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return fmt.Errorf("%q has a name ending in a dot or space, which Windows trims", p)
		}
		stem, _, _ := strings.Cut(part, ".")
		if reservedNames[strings.ToUpper(strings.TrimRight(stem, " "))] {
			return fmt.Errorf("%q uses the reserved device name %s", p, stem)
		}
	}
	return nil
}
