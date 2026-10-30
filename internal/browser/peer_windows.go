package browser

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// sioAFUnixGetPeerPID is SIO_AF_UNIX_GETPEERPID: _WSAIOR(IOC_VENDOR, 256).
const sioAFUnixGetPeerPID = 0x40000000 | 0x18000000 | 256

// peerCheck accepts only a process of this user (SPEC §21): it reads the
// peer's process ID from the socket and compares its token's user with
// ours.
func peerCheck(conn net.Conn) error {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return errors.New("not a socket")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var pid, n uint32
	var ioErr error
	if err := raw.Control(func(fd uintptr) {
		ioErr = syscall.WSAIoctl(syscall.Handle(fd), sioAFUnixGetPeerPID, nil, 0,
			(*byte)(unsafe.Pointer(&pid)), uint32(unsafe.Sizeof(pid)), &n, nil, 0)
	}); err != nil {
		return err
	}
	if ioErr != nil {
		return fmt.Errorf("reading the peer's process: %w", ioErr)
	}
	const processQueryLimitedInformation = 0x1000
	proc, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return fmt.Errorf("opening the peer's process: %w", err)
	}
	defer syscall.CloseHandle(proc)
	var token syscall.Token
	if err := syscall.OpenProcessToken(proc, syscall.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	theirs, err := tokenUser(token)
	if err != nil {
		return err
	}
	own, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer own.Close()
	ours, err := tokenUser(own)
	if err != nil {
		return err
	}
	if theirs != ours {
		return fmt.Errorf("the peer runs as %s, not as this user", theirs)
	}
	return nil
}

func tokenUser(t syscall.Token) (string, error) {
	u, err := t.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String()
}
