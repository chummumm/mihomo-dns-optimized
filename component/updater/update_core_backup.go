package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// prepareCoreBackupContext leaves the persistent backup name untouched until
// the caller has rechecked the executable identity and is ready to install.
func (u *CoreUpdater) prepareCoreBackupContext(ctx context.Context, currentPath, backupDir string, expected coreExecutableSnapshot) (backupPath string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(backupDir, ".mihomo-backup-")
	if err != nil {
		return "", err
	}
	backupPath = temporary.Name()
	ownsBackupPath := true
	defer func() {
		if err != nil && ownsBackupPath {
			_ = os.Remove(backupPath)
		}
	}()
	if err = temporary.Close(); err != nil {
		return backupPath, err
	}
	if err = os.Remove(backupPath); err != nil {
		return backupPath, err
	}
	// Once the placeholder is removed, a failed exclusive creation does not
	// give this transaction ownership of anything now occupying that name.
	ownsBackupPath = false
	link := u.linkBackup
	if link == nil {
		link = os.Link
	}
	linkErr := link(currentPath, backupPath)
	if linkErr != nil {
		// Do not remove a path left by a failed link attempt before opening it:
		// O_EXCL also prevents a competing path/symlink from being overwritten.
		if err = copyCoreBackupContext(ctx, currentPath, backupPath); err != nil {
			return backupPath, fmt.Errorf("copy core backup after hard link failed (%v): %w", linkErr, err)
		}
	}
	ownsBackupPath = true
	// Rechecking only currentPath would miss a temporary replacement restored
	// after linking/copying. Validate the backup itself against the original
	// snapshot before either persistent name can be replaced.
	actual, err := snapshotCoreExecutable(ctx, backupPath)
	if err != nil {
		return backupPath, err
	}
	if linkErr == nil {
		if !expected.equal(actual) {
			return backupPath, errors.New("current executable changed while preparing backup")
		}
	} else if expected.info.Size() != actual.info.Size() || expected.info.Mode().Perm() != actual.info.Mode().Perm() || expected.digest != actual.digest {
		return backupPath, errors.New("current executable changed while preparing backup")
	}
	return backupPath, nil
}

func copyCoreBackupContext(ctx context.Context, currentPath, backupPath string) (err error) {
	source, err := os.Open(currentPath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCoreFileSize {
		return fmt.Errorf("invalid executable backup size or type")
	}
	backup, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := backup.Close(); err == nil {
			err = closeErr
		}
		// O_EXCL succeeded, so this function owns partial-copy cleanup,
		// including Sync or Close failures, until it returns successfully.
		if err != nil {
			_ = os.Remove(backupPath)
		}
	}()
	n, err := io.Copy(backup, io.LimitReader(coreContextReader{ctx, source}, maxCoreFileSize+1))
	if err != nil {
		return err
	}
	if n == 0 || n > maxCoreFileSize {
		return fmt.Errorf("invalid executable backup size: %d", n)
	}
	if err = backup.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	return backup.Sync()
}
