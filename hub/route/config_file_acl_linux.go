package route

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

const configFileAccessACL = "system.posix_acl_access"

func configFileACLAbsent(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP)
}

func readConfigFileAccessACL(file *os.File) ([]byte, error) {
	size, err := unix.Fgetxattr(int(file.Fd()), configFileAccessACL, nil)
	if configFileACLAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if size < 0 || size > 65536 {
		return nil, os.ErrInvalid
	}
	if size == 0 {
		return nil, nil
	}
	acl := make([]byte, size)
	length, err := unix.Fgetxattr(int(file.Fd()), configFileAccessACL, acl)
	if err != nil {
		return nil, err
	}
	return acl[:length], nil
}

func preserveConfigFileAccessACL(file *os.File, acl []byte) error {
	if len(acl) > 0 {
		return unix.Fsetxattr(int(file.Fd()), configFileAccessACL, acl, 0)
	}
	// CreateTemp can inherit the directory's default ACL. Remove that ACL if
	// the original had none, otherwise chmod can unintentionally grant named
	// users/group entries access to configuration secrets after replacement.
	err := unix.Fremovexattr(int(file.Fd()), configFileAccessACL)
	if configFileACLAbsent(err) {
		return nil
	}
	return err
}
