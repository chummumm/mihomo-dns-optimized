//go:build !linux && !windows

package route

import (
	"os"

	"golang.org/x/sys/unix"
)

func configFileOwnershipPrivileges() (chown, fowner, fsetid bool) {
	privileged := os.Geteuid() == 0
	return privileged, privileged, privileged
}

func configFileEffectiveAccess(path string, mode uint32) error {
	return unix.Faccessat(unix.AT_FDCWD, path, mode, unix.AT_EACCESS)
}

func configFilePlatformWritable(_ *os.File, _ os.FileInfo, _ os.FileInfo) bool {
	return true
}
