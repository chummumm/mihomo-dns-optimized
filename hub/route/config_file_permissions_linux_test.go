package route

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/metacubex/http"
	C "github.com/metacubex/mihomo/constant"
	"golang.org/x/sys/unix"
)

const (
	configFilePermissionChild    = "MIHOMO_CONFIG_FILE_PERMISSION_CHILD"
	configFilePermissionRequired = "MIHOMO_CONFIG_FILE_PERMISSION_REQUIRED"
)

func configFilePermissionSkip(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv(configFilePermissionRequired) == "1" {
		t.Fatal("required permission regression cannot run: " + reason)
	}
	t.Skip(reason)
}

type configFilePermissionCase struct {
	name                       string
	fileUID, fileGID           int
	directoryUID, directoryGID int
	fileMode, directoryMode    os.FileMode
	readable, writable         bool
	aclPermission              int
}

func configFilePermissionCases() []configFilePermissionCase {
	return []configFilePermissionCase{
		{"root-owned", 0, 0, 65534, 65534, 0644, 0700, true, false, -1},
		{"foreign-owner-writable-mode", 0, 0, 65534, 65534, 0666, 0700, true, false, -1},
		{"owned-read-only", 65534, 65534, 65534, 65534, 0440, 0700, true, false, -1},
		{"parent-read-only", 65534, 65534, 65534, 65534, 0640, 0500, true, false, -1},
		{"owned-writable", 65534, 65534, 65534, 65534, 0640, 0700, true, true, -1},
		{"foreign-group", 65534, 65533, 65534, 65534, 0640, 0700, true, false, -1},
		{"inherited-group", 65534, 65533, 65534, 65533, 0640, os.ModeSetgid | 0700, true, true, -1},
		{"inherited-group-setgid-file", 65534, 65533, 65534, 65533, os.ModeSetgid | 0640, os.ModeSetgid | 0700, true, false, -1},
		{"sticky-foreign-owner", 0, 0, 0, 0, 0666, os.ModeSticky | 0777, true, false, -1},
		{"no-read-access", 0, 0, 65534, 65534, 0600, 0700, false, false, -1},
		{"parent-acl-allow", 65534, 65534, 0, 0, 0640, 0700, true, true, 7},
		{"parent-acl-deny", 65534, 65534, 0, 0, 0640, 0777, true, false, 5},
	}
}

// Exercise the same unprivileged UID as a service even when CI runs as root.
// Root can read/write nearly every mode-bit fixture, hiding the original bug.
func TestConfigFileUnprivilegedPermissions(t *testing.T) {
	if directory := os.Getenv(configFilePermissionChild); directory != "" {
		configFileCheckUnprivilegedPermissions(t, directory)
		return
	}
	if os.Geteuid() != 0 {
		configFilePermissionSkip(t, "foreign-owner fixtures require root; direct permission-loss tests still run as this user")
	}
	directory, err := os.MkdirTemp("", "mihomo-config-permissions-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range configFilePermissionCases() {
		parent := filepath.Join(directory, test.name)
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "config.yaml")
		if err := os.WriteFile(path, []byte("mode: rule\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, item := range []struct {
			path     string
			uid, gid int
			mode     os.FileMode
		}{{path, test.fileUID, test.fileGID, test.fileMode}, {parent, test.directoryUID, test.directoryGID, test.directoryMode}} {
			if err := os.Chown(item.path, item.uid, item.gid); err != nil {
				if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
					configFilePermissionSkip(t, "alternate UID/GID unavailable in this user namespace")
				}
				t.Fatal(err)
			}
			if err := os.Chmod(item.path, item.mode); err != nil {
				t.Fatal(err)
			}
		}
		if test.aclPermission >= 0 {
			entries := []struct {
				tag, permission uint16
				id              uint32
			}{
				{1, 7, ^uint32(0)}, {2, uint16(test.aclPermission), 65534},
				{4, 0, ^uint32(0)}, {16, 7, ^uint32(0)}, {32, uint16(test.directoryMode.Perm() & 7), ^uint32(0)},
			}
			acl := make([]byte, 4+8*len(entries))
			binary.LittleEndian.PutUint32(acl, 2)
			for i, entry := range entries {
				offset := 4 + i*8
				binary.LittleEndian.PutUint16(acl[offset:], entry.tag)
				binary.LittleEndian.PutUint16(acl[offset+2:], entry.permission)
				binary.LittleEndian.PutUint32(acl[offset+4:], entry.id)
			}
			if err := unix.Setxattr(parent, configFileAccessACL, acl, 0); err != nil {
				if configFileACLAbsent(err) || errors.Is(err, syscall.EPERM) {
					if os.Getenv(configFilePermissionRequired) == "1" {
						t.Fatalf("required permission regression needs POSIX ACLs: %v", err)
					}
					// Keep the other permission cases active on filesystems without ACLs.
					if err := os.RemoveAll(parent); err != nil {
						t.Fatal(err)
					}
					continue
				}
				t.Fatal(err)
			}
		}
	}
	// Go's own temporary build directory can be inaccessible to UID 65534.
	// Link/copy only this test executable into our deliberately traversable dir.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childExecutable := filepath.Join(directory, "config-file.test")
	if err := os.Link(executable, childExecutable); err != nil {
		source, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		target, err := os.OpenFile(childExecutable, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(target, source)
		closeErr := target.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	command := exec.Command(childExecutable, "-test.run=^TestConfigFileUnprivilegedPermissions$", "-test.v")
	command.Env = append(os.Environ(), configFilePermissionChild+"="+directory)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	output, err := command.CombinedOutput()
	if errors.Is(err, syscall.EPERM) {
		configFilePermissionSkip(t, "subprocess credentials unavailable in this user namespace")
	}
	if err != nil {
		t.Fatalf("unprivileged configuration tests failed: %v\n%s", err, output)
	}
	t.Logf("unprivileged process results:\n%s", output)
}

func configFileCheckUnprivilegedPermissions(t *testing.T, directory string) {
	if os.Geteuid() != 65534 || os.Getegid() != 65534 {
		t.Fatal("permission subprocess did not adopt UID/GID 65534")
	}
	capabilities, err := configFileCapabilities()
	if err != nil || capabilities[0].Effective != 0 || capabilities[1].Effective != 0 {
		t.Fatal("permission subprocess retained effective capabilities or could not verify their absence")
	}
	for _, test := range configFilePermissionCases() {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, test.name, "config.yaml")
			original, err := os.Stat(path)
			if os.IsNotExist(err) && test.aclPermission >= 0 {
				configFilePermissionSkip(t, "filesystem does not support POSIX ACLs")
			}
			if err != nil {
				t.Fatal(err)
			}
			owner := original.Sys().(*syscall.Stat_t)
			if int(owner.Uid) != test.fileUID || int(owner.Gid) != test.fileGID {
				t.Fatal("permission fixture does not have its requested owner/group")
			}
			parentBefore, err := os.Stat(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			oldPath := C.Path.Config()
			C.SetConfig(path)
			t.Cleanup(func() { C.SetConfig(oldPath) })
			response, document := requestConfigFile(t, http.MethodGet, nil)
			if !test.readable {
				assertConfigFilePermissionCode(t, response.Code, response.Body.Bytes(), "config_file_access_denied")
				if strings.Contains(response.Body.String(), "mode: rule") {
					t.Fatal("unreadable response disclosed file content")
				}
				return
			}
			if response.Code != http.StatusOK || document.Content != "mode: rule\n" || document.Writable != test.writable {
				t.Fatalf("incorrect read capability: status=%d writable=%v expected=%v", response.Code, document.Writable, test.writable)
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &fields) != nil || fields["writable"] == nil {
				t.Fatal("GET omitted writable capability")
			}
			parentAfter, err := os.Stat(filepath.Dir(path))
			if err != nil || parentBefore.ModTime() != parentAfter.ModTime() {
				t.Fatal("capability check wrote into the configuration directory")
			}
			response, saved := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", document.Revision))
			if test.writable {
				if response.Code != http.StatusOK || !saved.Writable {
					t.Fatalf("writable file could not be saved: %d %s", response.Code, response.Body.String())
				}
				assertConfigBytes(t, path, "mode: direct\n")
			} else {
				assertConfigFilePermissionCode(t, response.Code, response.Body.Bytes(), "config_file_read_only")
				response, _ = requestConfigFile(t, http.MethodPut, saveConfigRequest(document.Content, document.Revision))
				assertConfigFilePermissionCode(t, response.Code, response.Body.Bytes(), "config_file_read_only")
				assertConfigBytes(t, path, document.Content)
			}
			current, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			before, after := original.Sys().(*syscall.Stat_t), current.Sys().(*syscall.Stat_t)
			if original.Mode() != current.Mode() || before.Uid != after.Uid || before.Gid != after.Gid {
				t.Fatal("capability check or save changed ownership/permissions")
			}
			if !test.writable && (!os.SameFile(original, current) || original.ModTime() != current.ModTime()) {
				t.Fatal("read-only save replaced the original file")
			}
		})
	}
	t.Run("permission-loss-after-read", TestConfigFilePermissionLossAfterRead)
}

func assertConfigFilePermissionCode(t *testing.T, status int, body []byte, code string) {
	t.Helper()
	var failure struct {
		Code string `json:"code"`
	}
	if status != http.StatusForbidden || json.Unmarshal(body, &failure) != nil || failure.Code != code {
		t.Fatalf("expected permission code %q, status=%d body=%s", code, status, body)
	}
}

func TestConfigFilePermissionLossAfterRead(t *testing.T) {
	for _, change := range []string{"file", "directory", "unreadable"} {
		t.Run(change, func(t *testing.T) {
			path := configFileFixture(t, "mode: rule\n")
			_, initial := requestConfigFile(t, http.MethodGet, nil)
			if !initial.Writable {
				t.Fatal("fresh file was not writable")
			}
			snapshot, failure := readCurrentConfigFile()
			if failure != nil {
				t.Fatal(failure.message)
			}
			changed, mode := path, os.FileMode(0440)
			if change == "directory" {
				changed, mode = filepath.Dir(path), 0500
			}
			if change == "unreadable" {
				mode = 0
			}
			if err := os.Chmod(changed, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(changed, 0700) })
			if configFileEffectiveAccess(changed, unix.W_OK) == nil {
				configFilePermissionSkip(t, "this process can override the changed permissions; the UID 65534 subprocess covers this case")
			}
			response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", initial.Revision))
			assertConfigFilePermissionCode(t, response.Code, response.Body.Bytes(), "config_file_read_only")
			// Also simulate permission loss after the save's initial snapshot.
			_, failure = saveConfigFile(snapshot, "mode: direct\n")
			if failure != configFileReadOnly {
				t.Fatalf("permission loss after snapshot returned %+v", failure)
			}
			if err := os.Chmod(changed, 0700); err != nil {
				t.Fatal(err)
			}
			assertConfigBytes(t, path, "mode: rule\n")
		})
	}
}

func TestConfigFilePermissionErrorClassification(t *testing.T) {
	for _, cause := range []error{os.ErrPermission, syscall.EACCES, syscall.EPERM, syscall.EROFS} {
		err := &os.PathError{Op: "save", Path: "private-path", Err: cause}
		if configFileStorageFailure(err, configFileSaveError) != configFileReadOnly ||
			configFileStorageFailure(err, configFileReadError) != configFileAccessDenied {
			t.Fatalf("permission error %v lost its structured classification", cause)
		}
	}
	if configFileStorageFailure(syscall.ENOSPC, configFileSaveError) != configFileSaveError {
		t.Fatal("disk-full error mislabeled as read-only")
	}
}
