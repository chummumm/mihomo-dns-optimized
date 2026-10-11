package route

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type configFileMetadata struct {
	identity string
	security *windows.SECURITY_DESCRIPTOR
}

// Directory access masks from WinNT.h; x/sys v0.30 does not export these names.
const (
	configFileWindowsAddFile     = 0x0002 // FILE_ADD_FILE (same bit as FILE_WRITE_DATA)
	configFileWindowsDeleteChild = 0x0040 // FILE_DELETE_CHILD
)

var configFileWindowsAccessCheck = windows.NewLazySystemDLL("advapi32.dll").NewProc("AccessCheck")

func openConfigFile(path string) (*os.File, error) {
	return openConfigFileWindows(path, windows.GENERIC_READ, false)
}

func openConfigFileWindows(path string, access uint32, directory bool) (*os.File, error) {
	namePath := path
	// Preserve os.Open's support for long absolute paths, including UNC paths,
	// when the host has not enabled long-path-aware Win32 file operations.
	if len(path) >= 248 && filepath.IsAbs(path) && !strings.HasPrefix(path, `\\?\`) && !strings.HasPrefix(path, `\??\`) && !strings.HasPrefix(path, `\\.\`) {
		if strings.HasPrefix(path, `\\`) {
			namePath = `\\?\UNC\` + path[2:]
		} else {
			namePath = `\\?\` + path
		}
	}
	name, err := windows.UTF16PtrFromString(namePath)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	// Share deletion so the capability check does not conflict with our own
	// reader. Snapshot identity/path checks still detect external replacement.
	handle, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func configFileWindowsAccess(path string, info os.FileInfo, access uint32) bool {
	openAccess := access
	if info.IsDir() {
		openAccess |= windows.READ_CONTROL
	}
	file, err := openConfigFileWindows(path, openAccess, info.IsDir())
	if err != nil {
		return false
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		return false
	}
	if !info.IsDir() {
		return true
	}
	// Opening a directory requires BACKUP_SEMANTICS, which can bypass its DACL
	// when backup/restore privileges are enabled. Creating our temporary file
	// does not use backup semantics: check the caller's ordinary access too.
	return configFileWindowsDirectoryAccess(file, access)
}

func configFileWindowsDirectoryAccess(file *os.File, access uint32) bool {
	security, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || configFileWindowsAccessCheck.Find() != nil {
		return false
	}
	// AccessCheck requires an impersonation token. Copy the effective token;
	// never impersonate or enable privileges on the process or calling thread.
	var effective windows.Token
	err = windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, true, &effective)
	if err == windows.ERROR_NO_TOKEN {
		err = windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &effective)
	}
	if err != nil {
		return false
	}
	defer effective.Close()
	var token windows.Token
	if err := windows.DuplicateTokenEx(effective, windows.TOKEN_QUERY, nil,
		windows.SecurityIdentification, windows.TokenImpersonation, &token); err != nil {
		return false
	}
	defer token.Close()

	// GENERIC_MAPPING and PRIVILEGE_SET use DWORD-aligned fields on every
	// Windows architecture. Callers request specific FILE_* rights; the mapping
	// supplies the equivalent masks for any generic rights in the descriptor.
	mapping := struct{ Read, Write, Execute, All uint32 }{
		windows.FILE_GENERIC_READ,
		windows.FILE_GENERIC_WRITE,
		windows.FILE_GENERIC_EXECUTE,
		windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x01ff, // FILE_ALL_ACCESS
	}
	privileges := make([]uint32, 8)
	for {
		size := uint32(len(privileges) * 4)
		var granted, allowed uint32
		ok, _, callErr := configFileWindowsAccessCheck.Call(
			uintptr(unsafe.Pointer(security)), uintptr(token), uintptr(access),
			uintptr(unsafe.Pointer(&mapping)), uintptr(unsafe.Pointer(&privileges[0])),
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&granted)), uintptr(unsafe.Pointer(&allowed)))
		if ok != 0 {
			return allowed != 0 && granted&access == access
		}
		if callErr != windows.ERROR_INSUFFICIENT_BUFFER || size <= uint32(len(privileges)*4) || size > 65536 {
			return false
		}
		privileges = make([]uint32, (size+3)/4)
	}
}

func configFileWindowsPrivilegeEnabled(token windows.Token, name string) bool {
	var privilege windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &privilege); err != nil {
		return false
	}
	var size uint32
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, nil, 0, &size); err != windows.ERROR_INSUFFICIENT_BUFFER || size < 4 || size > 65536 {
		return false
	}
	data := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, &data[0], uint32(len(data)), &size); err != nil || size < 4 || size > uint32(len(data)) {
		return false
	}
	// TOKEN_PRIVILEGES contains a DWORD count followed by 12-byte
	// LUID_AND_ATTRIBUTES records on both 32- and 64-bit Windows.
	count := binary.LittleEndian.Uint32(data)
	if count > (size-4)/12 {
		return false
	}
	for i := uint32(0); i < count; i++ {
		entry := data[4+i*12 : 4+(i+1)*12]
		if binary.LittleEndian.Uint32(entry) == privilege.LowPart &&
			int32(binary.LittleEndian.Uint32(entry[4:])) == privilege.HighPart &&
			binary.LittleEndian.Uint32(entry[8:])&windows.SE_PRIVILEGE_ENABLED != 0 {
			return true
		}
	}
	return false
}

func configFileWindowsOwnerAssignable(owner *windows.SID) bool {
	if owner == nil {
		return false
	}
	token := windows.GetCurrentThreadEffectiveToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return false
	}
	if owner.Equals(user.User.Sid) {
		return true
	}
	groups, err := token.GetTokenGroups()
	if err == nil {
		for _, group := range groups.AllGroups() {
			if group.Attributes&(windows.SE_GROUP_ENABLED|windows.SE_GROUP_OWNER) == windows.SE_GROUP_ENABLED|windows.SE_GROUP_OWNER &&
				group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 && owner.Equals(group.Sid) {
				return true
			}
		}
	}
	// Do not enable privileges to make an otherwise read-only file editable.
	return configFileWindowsPrivilegeEnabled(token, "SeRestorePrivilege")
}

func configFileWritable(file *os.File, info os.FileInfo, metadata configFileMetadata) bool {
	if info.Mode().Perm()&0200 == 0 || metadata.security == nil {
		return false
	}
	var filesystemFlags uint32
	if err := windows.GetVolumeInformationByHandle(windows.Handle(file.Fd()), nil, 0, nil, nil, &filesystemFlags, nil, 0); err == nil && filesystemFlags&windows.FILE_READ_ONLY_VOLUME != 0 {
		return false
	}
	owner, _, err := metadata.security.Owner()
	if err != nil || !configFileWindowsOwnerAssignable(owner) || !configFileWindowsAccess(file.Name(), info, windows.GENERIC_WRITE) {
		return false
	}
	directory := filepath.Dir(file.Name())
	parent, err := os.Lstat(directory)
	if err != nil || !parent.IsDir() || !configFileWindowsAccess(directory, parent, configFileWindowsAddFile) {
		return false
	}
	// Replacing an existing file requires DELETE on the file OR
	// FILE_DELETE_CHILD on its parent, in addition to creating the temporary.
	return configFileWindowsAccess(file.Name(), info, windows.DELETE) ||
		configFileWindowsAccess(directory, parent, configFileWindowsDeleteChild)
}

func configFilePermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, windows.ERROR_WRITE_PROTECT) ||
		errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_INVALID_OWNER) ||
		errors.Is(err, windows.ERROR_INVALID_PRIMARY_GROUP) || errors.Is(err, windows.ERROR_CLOUD_FILE_READ_ONLY_VOLUME)
}

func readConfigFileMetadata(file *os.File, _ os.FileInfo) (configFileMetadata, error) {
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &identity); err != nil {
		return configFileMetadata{}, err
	}
	security, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return configFileMetadata{}, err
	}
	return configFileMetadata{
		identity: fmt.Sprintf("%d:%d:%d:%s", identity.VolumeSerialNumber, identity.FileIndexHigh, identity.FileIndexLow, security.String()),
		security: security,
	}, nil
}

func preserveConfigFileMetadata(file *os.File, info os.FileInfo, metadata configFileMetadata) error {
	owner, _, err := metadata.security.Owner()
	if err != nil {
		return err
	}
	group, _, err := metadata.security.Group()
	if err != nil {
		return err
	}
	dacl, _, err := metadata.security.DACL()
	if err != nil {
		return err
	}
	control, _, err := metadata.security.Control()
	if err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		flags |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	// Preserve access controls instead of silently inheriting broader access from
	// the directory. If the service lacks permission, keep the original intact.
	if err := windows.SetNamedSecurityInfo(file.Name(), windows.SE_FILE_OBJECT, flags, owner, group, dacl, nil); err != nil {
		return err
	}
	return file.Chmod(info.Mode())
}
