package route

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/metacubex/mihomo/component/updater"
	C "github.com/metacubex/mihomo/constant"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
)

func TestUIUpdaterPrivatePathsAreNotServedOrListed(t *testing.T) {
	oldHome := C.Path.HomeDir()
	C.SetHomeDir(t.TempDir())
	directory := updater.UIPath()
	t.Cleanup(func() { C.SetHomeDir(oldHome) })
	// Old integrations cannot retarget static serving away from HomeDir/ui.
	SetUIPath(t.TempDir())
	for _, name := range []string{
		".mihomo-ui-stage-one/archive",
		".mihomo-ui-previous-two/index.html",
		".mihomo-ui-backup-three/previous/index.html",
		".mihomo-ui-retained.json",
		"nested/.mihomo-ui-stage-four/archive",
		"nested/.mihomo-ui-backup-five/previous/index.html",
		"nested/.mihomo-ui-retained.json",
		"nested/visible.txt", ".ordinary-dotfile", "visible.txt",
	} {
		full := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		content := "public fixture"
		if privateUIUpdaterPath(name) {
			content = "private updater fixture"
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	handler := router(false, "controller-secret", "", Cors{})
	for _, target := range []string{
		"/ui/.mihomo-ui-stage-one/archive",
		"/ui/.mihomo-ui-previous-two/",
		"/ui/.mihomo-ui-backup-three/previous/",
		"/ui/.mihomo-ui-retained.json",
		"/ui/nested/.mihomo-ui-stage-four/archive",
		"/ui/nested/.mihomo-ui-backup-five/previous/",
		"/ui/nested/.mihomo-ui-retained.json",
		"/ui/%2Emihomo-ui-stage-one%2Farchive",
		"/ui/nested%2F.mihomo-ui-stage-four%2Farchive",
		"/ui/nested%5C.MIHOMO-UI-STAGE-four%5Carchive",
		"/ui/.MIHOMO-UI-RETAINED.JSON.%20",
		"/ui/.mihomo-ui-retained.json%3A%24DATA",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "private updater fixture") {
			t.Fatalf("private updater path was not hidden: %s, status %d", target, response.Code)
		}
	}
	for _, target := range []string{"/ui/", "/ui/nested/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), ".mihomo-ui-") || !strings.Contains(response.Body.String(), "visible.txt") {
			t.Fatalf("public listing revealed updater paths: %s, status %d", target, response.Code)
		}
	}
	for _, target := range []string{"/ui/visible.txt", "/ui/nested/visible.txt", "/ui/.ordinary-dotfile"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK || response.Body.String() != "public fixture" {
			t.Fatalf("normal static file behavior changed: %s, status %d", target, response.Code)
		}
	}
}

func TestUIUpdaterPrivatePathAliasesAndDirectoryPaging(t *testing.T) {
	directory := t.TempDir()
	private := filepath.Join(directory, ".mihomo-ui-stage-private")
	if err := os.Mkdir(private, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "archive"), []byte("private"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "visible.txt"), []byte("public"), 0644); err != nil {
		t.Fatal(err)
	}
	filesystem := uiFileSystem{root: directory}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(private, filepath.Join(directory, "alias")); err != nil {
			t.Fatal(err)
		}
		if _, err := filesystem.Open("alias/archive"); !os.IsNotExist(err) {
			t.Fatal("symlink alias exposed updater staging")
		}
	}
	file, err := filesystem.Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	seen := false
	for {
		entries, err := file.Readdir(1)
		for _, entry := range entries {
			if privateUIUpdaterPath(entry.Name()) {
				t.Fatal("paged directory read returned an internal updater path")
			}
			seen = seen || entry.Name() == "visible.txt"
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !seen {
		t.Fatal("paged directory read lost ordinary files")
	}
}
