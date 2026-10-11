//go:build !windows

package route

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/metacubex/http"
	"golang.org/x/sys/unix"
)

func TestConfigFilePreservesUnixOwnership(t *testing.T) {
	path := configFileFixture(t, "mode: rule\n")
	// When running as root, use different IDs so an omitted Chown cannot pass
	// merely because the temporary file has the process's default ownership.
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 65534, 65534); err != nil {
			if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
				t.Log("alternate UID/GID unavailable in this user namespace; checking current ownership")
			} else {
				t.Fatal(err)
			}
		}
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	originalStat := original.Sys().(*syscall.Stat_t)
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", initial.Revision))
	if response.Code != http.StatusOK {
		t.Fatalf("save failed: %d", response.Code)
	}
	saved, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	savedStat := saved.Sys().(*syscall.Stat_t)
	if savedStat.Uid != originalStat.Uid || savedStat.Gid != originalStat.Gid || saved.Mode() != original.Mode() {
		t.Fatal("configuration save changed Unix ownership or permissions")
	}
}

func TestConfigFileDirectoryPermissionFailurePreservesOriginal(t *testing.T) {
	original := "mode: rule\n"
	path := configFileFixture(t, original)
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	directory := filepath.Dir(path)
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	if configFileEffectiveAccess(directory, unix.W_OK|unix.X_OK) == nil {
		t.Skip("this process can override directory permission checks")
	}
	response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", initial.Revision))
	if response.Code != http.StatusForbidden {
		t.Fatalf("unwritable directory save was not rejected: %d", response.Code)
	}
	_, current := requestConfigFile(t, http.MethodGet, nil)
	if current.Writable || current.Content != original {
		t.Fatal("unwritable directory did not expose a readable, read-only document")
	}
	assertConfigBytes(t, path, original)
}

func TestConfigFileRejectsFIFOBeforeOpening(t *testing.T) {
	path := configFileFixture(t, "mode: rule\n")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	response, _ := requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatal("FIFO configuration was not rejected before opening")
	}
}
