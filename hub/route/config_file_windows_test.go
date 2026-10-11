package route

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metacubex/http"
	C "github.com/metacubex/mihomo/constant"
	"golang.org/x/sys/windows"
)

func configFileWindowsDeny(t *testing.T, path string, mask windows.ACCESS_MASK) {
	t.Helper()
	security, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := security.DACL()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := security.Control()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask,
		AccessMode:        windows.DENY_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, original)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
		if control&windows.SE_DACL_PROTECTED != 0 {
			flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags, nil, nil, original, nil); err != nil {
			t.Error(err)
		}
	})
}

func TestConfigFileWindowsWritableCheckLeavesOriginalUntouched(t *testing.T) {
	original := "mode: rule\n"
	path := configFileFixture(t, original)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	response, initial := requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusOK || !initial.Writable {
		t.Fatalf("ordinary writable file was not editable: status=%d writable=%v", response.Code, initial.Writable)
	}
	response, repeat := requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusOK || !repeat.Writable || initial.Revision != repeat.Revision {
		t.Fatal("permission inspection changed the document revision or editability")
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.ModTime() != after.ModTime() {
		t.Fatal("permission inspection changed the file identity, mode or timestamp")
	}
	assertConfigBytes(t, path, original)
	response, saved := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", initial.Revision))
	if response.Code != http.StatusOK || !saved.Writable {
		t.Fatalf("writable preflight did not allow a safe save: status=%d", response.Code)
	}
}

func TestConfigFileWindowsLongPathRemainsReadable(t *testing.T) {
	path := configFileFixture(t, "mode: rule\n")
	directory := filepath.Dir(path)
	for len(directory) < 280 {
		directory = filepath.Join(directory, strings.Repeat("config", 10))
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("mode: rule\n"), 0640); err != nil {
		t.Fatal(err)
	}
	C.SetConfig(path)
	response, document := requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusOK || !document.Writable || document.Path != path || document.Content != "mode: rule\n" {
		t.Fatalf("long configuration path no longer readable/editable: status=%d", response.Code)
	}
	assertConfigBytes(t, path, "mode: rule\n")
}

func TestConfigFileWindowsReadOnlyBeforeEditing(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		configure func(*testing.T, string)
	}{
		{"read-only attribute", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0640) })
		}},
		{"file ACL", func(t *testing.T, path string) {
			configFileWindowsDeny(t, path, windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA)
		}},
		{"directory ACL", func(t *testing.T, path string) {
			configFileWindowsDeny(t, filepath.Dir(path), configFileWindowsAddFile)
		}},
		{"replacement ACL", func(t *testing.T, path string) {
			configFileWindowsDeny(t, path, windows.DELETE)
			configFileWindowsDeny(t, filepath.Dir(path), configFileWindowsDeleteChild)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			original := "mode: rule\n"
			path := configFileFixture(t, original)
			scenario.configure(t, path)
			response, document := requestConfigFile(t, http.MethodGet, nil)
			if response.Code != http.StatusOK || document.Writable || document.Content != original {
				t.Fatalf("read-only file was not readable with writable=false: status=%d writable=%v", response.Code, document.Writable)
			}
			for _, content := range []string{original, "mode: direct\n"} {
				response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest(content, document.Revision))
				var failure struct{ Code string }
				if response.Code != http.StatusForbidden || json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Code != "config_file_read_only" {
					t.Fatalf("read-only save lacks its distinct error: status=%d body=%s", response.Code, response.Body.String())
				}
			}
			assertConfigBytes(t, path, original)
		})
	}
}

func TestConfigFileWindowsReplacementAcceptsEitherDeleteRight(t *testing.T) {
	for _, denyOnFile := range []bool{true, false} {
		name := "file grants delete"
		if denyOnFile {
			name = "parent grants delete child"
		}
		t.Run(name, func(t *testing.T) {
			path := configFileFixture(t, "mode: rule\n")
			if denyOnFile {
				configFileWindowsDeny(t, path, windows.DELETE)
			} else {
				configFileWindowsDeny(t, filepath.Dir(path), configFileWindowsDeleteChild)
			}
			response, document := requestConfigFile(t, http.MethodGet, nil)
			if response.Code != http.StatusOK || !document.Writable {
				t.Fatalf("alternate replacement permission was rejected: status=%d writable=%v", response.Code, document.Writable)
			}
			response, _ = requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", document.Revision))
			if response.Code != http.StatusOK {
				t.Fatalf("alternate replacement permission failed during save: status=%d", response.Code)
			}
			assertConfigBytes(t, path, "mode: direct\n")
		})
	}
}

func TestConfigFileWindowsPermissionFailuresAreDistinct(t *testing.T) {
	for _, failure := range []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_WRITE_PROTECT,
		windows.ERROR_PRIVILEGE_NOT_HELD, windows.ERROR_INVALID_OWNER, windows.ERROR_INVALID_PRIMARY_GROUP} {
		if !configFilePermissionError(&os.PathError{Op: "rename", Path: "config.yaml", Err: failure}) {
			t.Fatalf("permission failure was not classified: %v", failure)
		}
	}
	if configFilePermissionError(errors.New("unrelated I/O failure")) || configFilePermissionError(windows.ERROR_DISK_FULL) {
		t.Fatal("unrelated storage failure was classified as a permissions failure")
	}
}
