package route

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/metacubex/http"
	"golang.org/x/sys/unix"
)

func configFileTestACL() []byte {
	// Linux POSIX ACL xattr v2: owner rw, named user r, group none,
	// group class mask r, others none.
	entries := []struct {
		tag, permission uint16
		id              uint32
	}{{1, 6, ^uint32(0)}, {2, 4, uint32(os.Geteuid())}, {4, 0, ^uint32(0)}, {16, 4, ^uint32(0)}, {32, 0, ^uint32(0)}}
	data := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(data, 2)
	for i, entry := range entries {
		offset := 4 + 8*i
		binary.LittleEndian.PutUint16(data[offset:], entry.tag)
		binary.LittleEndian.PutUint16(data[offset+2:], entry.permission)
		binary.LittleEndian.PutUint32(data[offset+4:], entry.id)
	}
	return data
}

func configFileSetTestACL(t *testing.T, path, attribute string, acl []byte) {
	t.Helper()
	if err := unix.Setxattr(path, attribute, acl, 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
			t.Skip("POSIX ACLs unavailable in this filesystem/user namespace")
		}
		t.Fatal(err)
	}
}

func TestConfigFilePreservesLinuxAccessACL(t *testing.T) {
	path := configFileFixture(t, "mode: rule\n")
	acl := configFileTestACL()
	configFileSetTestACL(t, path, configFileAccessACL, acl)
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", initial.Revision))
	if response.Code != http.StatusOK {
		t.Fatalf("ACL-preserving save failed: %d", response.Code)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	actual, err := readConfigFileAccessACL(file)
	if err != nil || !bytes.Equal(actual, acl) {
		t.Fatal("save changed the original access ACL")
	}
}

func TestConfigFileDoesNotInheritAdditionalLinuxAccessACL(t *testing.T) {
	path := configFileFixture(t, "mode: rule\n")
	configFileSetTestACL(t, filepath.Dir(path), "system.posix_acl_default", configFileTestACL())
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: direct\n", initial.Revision))
	if response.Code != http.StatusOK {
		t.Fatalf("save in ACL directory failed: %d", response.Code)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	acl, err := readConfigFileAccessACL(file)
	if err != nil || len(acl) != 0 {
		t.Fatal("save inherited a new access ACL from the directory")
	}
}
