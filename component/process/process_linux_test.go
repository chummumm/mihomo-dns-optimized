//go:build linux && !android

package process

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessLookupSkipsExitedProcess(t *testing.T) {
	procRoot := t.TempDir()
	exitedPID := filepath.Join(procRoot, "100")
	livePID := filepath.Join(procRoot, "200")
	if err := os.Mkdir(exitedPID, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(livePID, "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[12345]", filepath.Join(livePID, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	const executable = "/test/bin/mihomo"
	if err := os.Symlink(executable, filepath.Join(livePID, "exe")); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(procRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "100" || entries[1].Name() != "200" {
		t.Fatalf("unexpected process entries: %v", entries)
	}
	// Keep the enumerated DirEntry, but make its subsequent Info call fail.
	// The surviving PID still owns the requested socket and executable link.
	if err := os.Remove(exitedPID); err != nil {
		t.Fatal(err)
	}
	if _, err := entries[0].Info(); !os.IsNotExist(err) {
		t.Fatalf("removed process entry must return not-exist: %v", err)
	}

	path, err := resolveProcessNameByProcEntries(procRoot, entries, 12345, uint32(os.Getuid()))
	if err != nil || path != executable {
		t.Fatalf("exited unrelated process hid the socket owner: path=%q err=%v", path, err)
	}
}

type processLookupInfoErrorEntry struct {
	os.DirEntry
	err error
}

func (entry processLookupInfoErrorEntry) Info() (os.FileInfo, error) {
	return nil, entry.err
}

func TestProcessLookupPreservesEntryInfoError(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(procRoot, "100"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		t.Fatal(err)
	}
	permissionError := &os.PathError{Op: "lstat", Path: filepath.Join(procRoot, "100"), Err: os.ErrPermission}
	entries[0] = processLookupInfoErrorEntry{DirEntry: entries[0], err: permissionError}

	path, err := resolveProcessNameByProcEntries(procRoot, entries, 12345, uint32(os.Getuid()))
	if path != "" || !errors.Is(err, permissionError) {
		t.Fatalf("non-transient process metadata error was changed: path=%q err=%v", path, err)
	}
}
