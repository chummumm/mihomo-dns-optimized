//go:build !windows

package route

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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

func configFilePermissionError(err error) bool {
	return os.IsPermission(err) || errors.Is(err, syscall.EROFS)
}

// Saving replaces an inode rather than writing in place. Both access to the
// current file and permission to create/replace its sibling are required, and
// the new inode must retain the original owner, group and ACL. Do not test this
// by creating files, or by changing ownership or permissions during a GET.
func configFileWritable(file *os.File, info os.FileInfo, metadata configFileMetadata) bool {
	path := file.Name()
	directory := filepath.Dir(path)
	if configFileEffectiveAccess(path, unix.W_OK) != nil ||
		configFileEffectiveAccess(directory, unix.W_OK|unix.X_OK) != nil {
		return false
	}
	parentInfo, err := os.Lstat(directory)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	parent, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	chown, fowner, fsetid := configFileOwnershipPrivileges()
	uid := os.Geteuid()
	if metadata.uid != uid && (!chown || !fowner) {
		// Chown alone is insufficient: after assigning a foreign owner,
		// restoring the ACL and mode also needs ownership override rights.
		return false
	}
	if parentInfo.Mode()&os.ModeSticky != 0 && int(parent.Uid) != uid && metadata.uid != uid && !fowner {
		return false
	}
	groupMember := metadata.gid == os.Getegid()
	if !groupMember {
		groups, err := os.Getgroups()
		if err != nil {
			return false
		}
		for _, gid := range groups {
			if gid == metadata.gid {
				groupMember = true
				break
			}
		}
	}
	inheritedGroup := parentInfo.Mode()&os.ModeSetgid != 0 && metadata.gid == int(parent.Gid)
	if !groupMember && !inheritedGroup && !chown {
		return false
	}
	// A group inherited from a setgid directory need not be in Getgroups().
	// In that case chmod may silently clear the original file's setgid bit.
	if info.Mode()&os.ModeSetgid != 0 && !groupMember && !fsetid {
		return false
	}
	return configFilePlatformWritable(file, info, parentInfo)
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
