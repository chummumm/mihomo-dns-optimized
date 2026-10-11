//go:build !windows

package route

import (
	"crypto/sha256"
	"fmt"
	"os"
	"syscall"
)

type configFileMetadata struct {
	identity  string
	uid       int
	gid       int
	accessACL []byte
}

func openConfigFile(path string) (*os.File, error) {
	// A replaced symlink/FIFO must not follow another target or block the API.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func readConfigFileMetadata(file *os.File, info os.FileInfo) (configFileMetadata, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return configFileMetadata{}, os.ErrInvalid
	}
	accessACL, err := readConfigFileAccessACL(file)
	if err != nil {
		return configFileMetadata{}, err
	}
	return configFileMetadata{
		identity: fmt.Sprintf("%d:%d:%d:%d:%x", stat.Dev, stat.Ino, stat.Uid, stat.Gid, sha256.Sum256(accessACL)),
		uid:      int(stat.Uid), gid: int(stat.Gid), accessACL: accessACL,
	}, nil
}

func preserveConfigFileMetadata(file *os.File, info os.FileInfo, metadata configFileMetadata) error {
	current, err := file.Stat()
	if err != nil {
		return err
	}
	owner, err := readConfigFileMetadata(file, current)
	if err != nil {
		return err
	}
	if owner.uid != metadata.uid || owner.gid != metadata.gid {
		if err := file.Chown(metadata.uid, metadata.gid); err != nil {
			return err
		}
	}
	// Clear inherited ACL entries before opening the temporary file's mode.
	// Chown/ACL changes can also affect mode, so restore it last.
	if err := preserveConfigFileAccessACL(file, metadata.accessACL); err != nil {
		return err
	}
	return file.Chmod(info.Mode())
}
