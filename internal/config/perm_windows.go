//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// On Windows, file modes don't restrict who may read a file; its access
// control list (DACL) does. Private here means a protected DACL, which
// inherits nothing from the parent directory and grants access only to the
// current user, SYSTEM and the Administrators group: the counterpart of
// 0600 and 0700, with SYSTEM and Administrators in the role of root.

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procConvertStringSecurityDescriptorToSecurityDescriptorW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetNamedSecurityInfoW                                = advapi32.NewProc("GetNamedSecurityInfoW")
)

const (
	sddlRevision1            = 1
	seFileObject             = 1
	ownerSecurityInformation = 0x1
	daclSecurityInformation  = 0x4

	sidSystem         = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"

	errorSharingViolation syscall.Errno = 32
)

// ACE types, flags and access rights from winnt.h.
const (
	accessAllowedAceType               = 0x0
	accessAllowedObjectAceType         = 0x5
	accessAllowedCallbackAceType       = 0x9
	accessAllowedCallbackObjectAceType = 0xb

	inheritOnlyAce = 0x8
	inheritedAce   = 0x10

	aceObjectTypePresent          = 0x1
	aceInheritedObjectTypePresent = 0x2

	fileReadData   = 0x1
	fileWriteData  = 0x2
	fileAppendData = 0x4
	writeDAC       = 0x40000
	writeOwner     = 0x80000
	maximumAllowed = 0x2000000
	genericAll     = 0x10000000
	genericWrite   = 0x40000000
	genericRead    = 0x80000000

	// exposingRights let an account read or change the content, or change
	// who may.
	exposingRights = fileReadData | fileWriteData | fileAppendData | writeDAC | writeOwner |
		maximumAllowed | genericAll | genericWrite | genericRead
)

// aclHeader and aceHeader mirror ACL and ACE_HEADER.
type aclHeader struct {
	AclRevision byte
	Sbz1        byte
	AclSize     uint16
	AceCount    uint16
	Sbz2        uint16
}

type aceHeader struct {
	AceType  byte
	AceFlags byte
	AceSize  uint16
}

func currentUserSID() (string, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String()
}

// privateSecurityAttributes returns security attributes carrying a private
// DACL. For a directory, the entries are inherited by everything created
// inside it. The caller must call free once the object is created.
func privateSecurityAttributes(dir bool) (sa *syscall.SecurityAttributes, free func(), err error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	sddl, err := syscall.UTF16PtrFromString(fmt.Sprintf("D:P(A;%[1]s;FA;;;%[2]s)(A;%[1]s;FA;;;SY)(A;%[1]s;FA;;;BA)", inherit, user))
	if err != nil {
		return nil, nil, err
	}
	var sd uintptr
	ok, _, callErr := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddl)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0)
	if ok == 0 {
		return nil, nil, fmt.Errorf("building a security descriptor: %w", callErr)
	}
	sa = &syscall.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, func() { syscall.LocalFree(syscall.Handle(sd)) }, nil
}

func mkdirPrivate(dir string) error {
	sa, free, err := privateSecurityAttributes(true)
	if err != nil {
		return err
	}
	defer free()
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := syscall.CreateDirectory(p, sa); err != nil {
		return &fs.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

func createPrivateFile(name string) (*os.File, error) {
	sa, free, err := privateSecurityAttributes(false)
	if err != nil {
		return nil, err
	}
	defer free()
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, sa,
		syscall.CREATE_NEW, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}

// replaceFile renames from over to. Virus scanners and the search indexer
// briefly open new files, which makes a rename fail, so it retries for a
// moment.
func replaceFile(from, to string) error {
	var err error
	for range 20 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) || (errno != syscall.ERROR_ACCESS_DENIED && errno != errorSharingViolation) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// syncDir does nothing: Windows has no way to flush a directory.
func syncDir(string) error { return nil }

// checkPrivate returns a warning when accounts other than the current user,
// SYSTEM and Administrators may read or change the file.
func checkPrivate(path string) (string, error) {
	owner, grants, nullDACL, err := fileACL(path)
	if err != nil {
		return "", err
	}
	if nullDACL {
		return fmt.Sprintf("%s has no access control list, so every account on this computer can read and change it%s",
			path, fixHint(path)), nil
	}
	user, err := currentUserSID()
	if err != nil {
		return "", err
	}
	trusted := map[string]bool{user: true, sidSystem: true, sidAdministrators: true}
	var others []string
	if !trusted[owner] {
		others = append(others, accountName(owner)+" (owner)")
	}
	for _, g := range grants {
		if g.mask&exposingRights != 0 && !trusted[g.sid] {
			if name := accountName(g.sid); !slices.Contains(others, name) {
				others = append(others, name)
			}
		}
	}
	if len(others) == 0 {
		return "", nil
	}
	return fmt.Sprintf("%s can be read or changed by other accounts (%s)%s", path, strings.Join(others, ", "), fixHint(path)), nil
}

func fixHint(path string) string {
	user := "%USERNAME%"
	if sid, err := currentUserSID(); err == nil {
		user = accountName(sid)
	}
	return fmt.Sprintf(`; the next save replaces it with an owner-only file, or restrict it now with: icacls "%s" /inheritance:r /grant:r "%s:F"`, path, user)
}

// grant is an access-allowed entry of a DACL.
type grant struct {
	sid   string
	flags byte
	mask  uint32
}

// fileACL returns the owner of a file and the access-allowed entries of its
// DACL that apply to the file itself. nullDACL reports a missing DACL, which
// grants everyone full access.
func fileACL(path string) (owner string, grants []grant, nullDACL bool, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", nil, false, err
	}
	var ownerSID, dacl unsafe.Pointer
	var sd uintptr
	r, _, _ := procGetNamedSecurityInfoW.Call(uintptr(unsafe.Pointer(p)), seFileObject,
		ownerSecurityInformation|daclSecurityInformation,
		uintptr(unsafe.Pointer(&ownerSID)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	if r != 0 {
		return "", nil, false, &fs.PathError{Op: "GetNamedSecurityInfo", Path: path, Err: syscall.Errno(r)}
	}
	defer syscall.LocalFree(syscall.Handle(sd))

	if ownerSID != nil {
		if owner, err = (*syscall.SID)(ownerSID).String(); err != nil {
			return "", nil, false, err
		}
	}
	if dacl == nil {
		return owner, nil, true, nil
	}
	acl := (*aclHeader)(dacl)
	off := unsafe.Sizeof(aclHeader{})
	for range acl.AceCount {
		if off+unsafe.Sizeof(aceHeader{}) > uintptr(acl.AclSize) {
			break
		}
		ace := unsafe.Add(dacl, off)
		hdr := (*aceHeader)(ace)
		if hdr.AceSize < uint16(unsafe.Sizeof(aceHeader{})) {
			break
		}
		off += uintptr(hdr.AceSize)
		if hdr.AceFlags&inheritOnlyAce != 0 {
			continue // applies only to objects created inside a directory
		}
		var sidOff uintptr
		switch hdr.AceType {
		case accessAllowedAceType, accessAllowedCallbackAceType:
			sidOff = 8 // header, mask
		case accessAllowedObjectAceType, accessAllowedCallbackObjectAceType:
			sidOff = 12 // header, mask, flags, then optional GUIDs
			objFlags := *(*uint32)(unsafe.Add(ace, 8))
			if objFlags&aceObjectTypePresent != 0 {
				sidOff += 16
			}
			if objFlags&aceInheritedObjectTypePresent != 0 {
				sidOff += 16
			}
		default:
			continue
		}
		sid, err := (*syscall.SID)(unsafe.Add(ace, sidOff)).String()
		if err != nil {
			return "", nil, false, err
		}
		grants = append(grants, grant{sid: sid, flags: hdr.AceFlags, mask: *(*uint32)(unsafe.Add(ace, 4))})
	}
	return owner, grants, false, nil
}

// accountName returns DOMAIN\name for a SID string, or the SID itself when
// it can't be looked up.
func accountName(sid string) string {
	s, err := syscall.StringToSid(sid)
	if err != nil {
		return sid
	}
	account, domain, _, err := s.LookupAccount("")
	switch {
	case err != nil:
		return sid
	case domain == "":
		return account
	}
	return domain + `\` + account
}
