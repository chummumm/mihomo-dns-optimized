package resource

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestResourceWritePublishesCompleteFilesAndPreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows can reject replacing an open destination; failed writes preserve the existing file")
	}
	path := filepath.Join(t.TempDir(), "provider.yaml")
	old, next := bytes.Repeat([]byte("a"), 65536), bytes.Repeat([]byte("b"), 65536)
	if err := os.WriteFile(path, old, 0o640); err != nil {
		t.Fatal(err)
	}
	stop, failures := make(chan struct{}), make(chan error, 1)
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				data, err := os.ReadFile(path)
				if err != nil {
					failures <- err
					return
				}
				if !bytes.Equal(data, old) && !bytes.Equal(data, next) {
					failures <- os.ErrInvalid
					return
				}
			}
		}
	}()
	for i := 0; i < 10; i++ {
		data := next
		if i%2 != 0 {
			data = old
		}
		if err := safeWrite(path, data); err != nil {
			close(stop)
			readers.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-failures:
		t.Fatalf("a reader observed an incomplete resource: %v", err)
	default:
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("file permissions changed: %o", info.Mode().Perm())
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".mihomo-resource-*"))
	if len(files) != 0 {
		t.Fatalf("temporary resource files remain: %v", files)
	}
}

func TestResourceWriteKeepsExistingSymlink(t *testing.T) {
	root := t.TempDir()
	target, link := filepath.Join(root, "actual"), filepath.Join(root, "rules")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := safeWrite(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replaced the configured symlink: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("symlink destination was not updated: %q %v", data, err)
	}
}
