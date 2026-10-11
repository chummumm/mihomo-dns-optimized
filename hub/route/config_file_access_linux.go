package route

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func configFileCapabilities() ([2]unix.CapUserData, error) {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	err := unix.Capget(&header, &data[0])
	return data, err
}

func configFileOwnershipPrivileges() (chown, fowner, fsetid bool) {
	data, err := configFileCapabilities()
	if err != nil {
		return false, false, false
	}
	return data[0].Effective&(1<<unix.CAP_CHOWN) != 0,
		data[0].Effective&(1<<unix.CAP_FOWNER) != 0,
		data[0].Effective&(1<<unix.CAP_FSETID) != 0
}

func configFileEffectiveAccess(path string, mode uint32) error {
	// Unlike a mode-bit approximation this consults the kernel's effective
	// credentials, POSIX ACLs, mount flags and security policy.
	err := unix.Faccessat2(unix.AT_FDCWD, path, mode, unix.AT_EACCESS)
	if err != unix.ENOSYS && err != unix.EPERM {
		return err
	}
	// Older kernels / syscall filters may lack faccessat2. access(2) still
	// checks ACLs, but uses real IDs and (for root) permitted capabilities.
	// Only use it when those credentials match the effective ones. x/sys's
	// userspace AT_EACCESS fallback would otherwise ignore POSIX ACLs.
	if os.Getuid() == os.Geteuid() && os.Getgid() == os.Getegid() {
		if os.Geteuid() != 0 {
			return unix.Access(path, mode)
		}
		data, err := configFileCapabilities()
		if err == nil && data[0].Effective == data[0].Permitted && data[1].Effective == data[1].Permitted {
			return unix.Access(path, mode)
		}
	}
	return os.ErrPermission
}

func configFilePlatformWritable(file *os.File, _ os.FileInfo, _ os.FileInfo) bool {
	// Append-only directories permit adding children, but forbid replacing an
	// existing name. Immutable / append-only files also cannot be replaced.
	const blocked = unix.STATX_ATTR_IMMUTABLE | unix.STATX_ATTR_APPEND
	for _, path := range []string{file.Name(), filepath.Dir(file.Name())} {
		var stat unix.Statx_t
		if unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS, &stat) == nil &&
			stat.Attributes&stat.Attributes_mask&blocked != 0 {
			return false
		}
	}
	// Some filesystems expose flags only through the older ioctl. Unsupported
	// flag queries are harmless: the actual save still classifies permission
	// failures without touching the original file.
	if flags, err := unix.IoctlGetInt(int(file.Fd()), unix.FS_IOC_GETFLAGS); err == nil && flags&blocked != 0 {
		return false
	}
	return true
}
