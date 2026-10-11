package route

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestUIUpdaterPrivateWindowsShortNames(t *testing.T) {
	directory := t.TempDir()
	filesystem := uiFileSystem{root: directory}
	for _, name := range []string{".mihomo-ui-stage-private", ".mihomo-ui-retained.json"} {
		t.Run(name, func(t *testing.T) {
			full := filepath.Join(directory, name)
			contents := full
			if name == ".mihomo-ui-stage-private" {
				if err := os.Mkdir(full, 0700); err != nil {
					t.Fatal(err)
				}
				contents = filepath.Join(full, "archive")
			}
			if err := os.WriteFile(contents, []byte("private updater fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			longPath, err := windows.UTF16PtrFromString(full)
			if err != nil {
				t.Fatal(err)
			}
			buffer := make([]uint16, 32768)
			length, err := windows.GetShortPathName(longPath, &buffer[0], uint32(len(buffer)))
			if err != nil || length == 0 || length >= uint32(len(buffer)) {
				t.Fatalf("cannot query Windows short path: %v", err)
			}
			shortName := filepath.Base(windows.UTF16ToString(buffer[:length]))
			if privateUIUpdaterPath(shortName) {
				t.Skip("8.3 aliases are disabled on this filesystem")
			}
			if contents != full {
				shortName = filepath.Join(shortName, "archive")
			}
			// Go's Windows EvalSymlinks normalizes each component through
			// FindFirstFile's FileName, including a real 8.3 alias.
			resolved, err := filepath.EvalSymlinks(filepath.Join(directory, shortName))
			if err != nil || !privateUIUpdaterPath(resolved) {
				t.Fatalf("Windows alias was not normalized to the private filename: %v", err)
			}
			if _, err := filesystem.Open(shortName); !os.IsNotExist(err) {
				t.Fatalf("Windows 8.3 alias exposed an updater file: %v", err)
			}
		})
	}
}
