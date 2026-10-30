//go:build windows

package install

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Windows keeps the user's PATH in the registry, as the value Path of
// HKEY_CURRENT_USER\Environment; programs started after it changes get the
// new one, once they are told it changed.
const environmentKey = `Environment`

var (
	advapi32               = syscall.NewLazyDLL("advapi32.dll")
	user32                 = syscall.NewLazyDLL("user32.dll")
	procRegCreateKeyEx     = advapi32.NewProc("RegCreateKeyExW")
	procRegSetValueEx      = advapi32.NewProc("RegSetValueExW")
	procRegDeleteKey       = advapi32.NewProc("RegDeleteKeyW")
	procSendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
)

func readUserPath() (string, bool, error) { return readValue(environmentKey, "Path") }

// writeUserPath sets the user's PATH and tells running programs, Explorer
// among them, that the environment changed, so the terminals they start
// from then on have it.
func writeUserPath(value string, expand bool) error {
	if err := writeValue(environmentKey, "Path", value, expand); err != nil {
		return err
	}
	env, _ := syscall.UTF16PtrFromString("Environment")
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001a, 0x0002
	var result uintptr
	procSendMessageTimeout.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 5000,
		uintptr(unsafe.Pointer(&result)))
	return nil
}

// readValue reads a text value of a key under HKEY_CURRENT_USER, and
// whether it is REG_EXPAND_SZ; "" when there is none.
func readValue(path, name string) (value string, expand bool, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", true, err
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, p, 0, syscall.KEY_QUERY_VALUE, &key); err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return "", true, nil
		}
		return "", true, err
	}
	defer syscall.RegCloseKey(key)
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", true, err
	}
	var typ, size uint32
	if err := syscall.RegQueryValueEx(key, n, nil, &typ, nil, &size); err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return "", true, nil
		}
		return "", true, err
	}
	if typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ {
		return "", true, fmt.Errorf(`HKCU\%s\%s isn't text`, path, name)
	}
	buf := make([]uint16, size/2+1)
	size = uint32(len(buf) * 2)
	if err := syscall.RegQueryValueEx(key, n, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return "", true, err
	}
	return syscall.UTF16ToString(buf), typ == syscall.REG_EXPAND_SZ, nil
}

// writeValue sets a text value of a key under HKEY_CURRENT_USER, creating
// the key.
func writeValue(path, name, value string, expand bool) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	data, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	var key syscall.Handle
	const keySetValue = 0x0002
	if r, _, _ := procRegCreateKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)), 0, 0, 0,
		keySetValue, 0, uintptr(unsafe.Pointer(&key)), 0); r != 0 {
		return syscall.Errno(r)
	}
	defer syscall.RegCloseKey(key)
	typ := uint32(syscall.REG_SZ)
	if expand {
		typ = syscall.REG_EXPAND_SZ
	}
	if r, _, _ := procRegSetValueEx.Call(uintptr(key), uintptr(unsafe.Pointer(n)), 0, uintptr(typ),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2)); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// deleteKey deletes a key, without subkeys, under HKEY_CURRENT_USER.
func deleteKey(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if r, _, _ := procRegDeleteKey.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p))); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}
