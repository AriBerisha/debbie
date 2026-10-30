package browser

import (
	"errors"
	"syscall"
	"unsafe"
)

// Chrome finds a user's native messaging host through the default value of
// HKCU\Software\Google\Chrome\NativeMessagingHosts\<name>: the path of its
// manifest (developer.chrome.com, "Native messaging").
const hostsKey = `Software\Google\Chrome\NativeMessagingHosts\`

var (
	advapi32           = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyEx = advapi32.NewProc("RegCreateKeyExW")
	procRegSetValueEx  = advapi32.NewProc("RegSetValueExW")
	procRegDeleteKey   = advapi32.NewProc("RegDeleteKeyW")
)

func registerHost(name, manifest string) error {
	return setUserDefault(hostsKey+name, manifest)
}

func unregisterHost(name string) error {
	return deleteUserKey(hostsKey + name)
}

// registeredHost returns the manifest path registered for the host; "" for
// none.
func registeredHost(name string) (string, error) {
	return userDefault(hostsKey + name)
}

// hostRegistration says where the host is registered, for messages.
func hostRegistration(name string) string { return `HKCU\` + hostsKey + name }

// setUserDefault sets the default value of a key under HKEY_CURRENT_USER,
// creating the key.
func setUserDefault(path, value string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	data, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	var key syscall.Handle
	const keySetValue = 0x0002
	r, _, _ := procRegCreateKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)), 0, 0, 0,
		keySetValue, 0, uintptr(unsafe.Pointer(&key)), 0)
	if r != 0 {
		return syscall.Errno(r)
	}
	defer syscall.RegCloseKey(key)
	r, _, _ = procRegSetValueEx.Call(uintptr(key), 0, 0, syscall.REG_SZ,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// userDefault returns the default value of a key under HKEY_CURRENT_USER;
// "" when there is no such key.
func userDefault(path string) (string, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, p, 0, syscall.KEY_QUERY_VALUE, &key); err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return "", nil
		}
		return "", err
	}
	defer syscall.RegCloseKey(key)
	var typ, size uint32
	if err := syscall.RegQueryValueEx(key, nil, nil, &typ, nil, &size); err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return "", nil
		}
		return "", err
	}
	if typ != syscall.REG_SZ || size < 2 {
		return "", nil
	}
	buf := make([]uint16, size/2)
	if err := syscall.RegQueryValueEx(key, nil, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return "", err
	}
	return syscall.UTF16ToString(buf), nil
}

// deleteUserKey deletes a key, without subkeys, under HKEY_CURRENT_USER;
// a missing key isn't an error.
func deleteUserKey(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r, _, _ := procRegDeleteKey.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)))
	if r != 0 && syscall.Errno(r) != syscall.ERROR_FILE_NOT_FOUND {
		return syscall.Errno(r)
	}
	return nil
}
