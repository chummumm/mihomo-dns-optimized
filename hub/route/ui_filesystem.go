package route

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/metacubex/http"
)

// Updater work/recovery directories can be siblings of an external-ui-name
// inside the static server's root. Filesystem permissions do not hide these
// files from the same process's unauthenticated FileServer.
func privateUIUpdaterPath(name string) bool {
	for _, component := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		// Match Windows case, trailing-dot/space and alternate-stream spelling
		// too. Only these updater-owned names are reserved; ordinary dotfiles
		// and other static content keep their existing serving behavior.
		component, _, _ = strings.Cut(component, ":")
		component = strings.ToLower(strings.TrimRight(component, " ."))
		if component == ".mihomo-ui-retained.json" ||
			strings.HasPrefix(component, ".mihomo-ui-stage-") ||
			strings.HasPrefix(component, ".mihomo-ui-previous-") ||
			strings.HasPrefix(component, ".mihomo-ui-backup-") {
			return true
		}
	}
	return false
}

type uiFileSystem struct{ root string }

func (s uiFileSystem) Open(name string) (http.File, error) {
	// FileServer passes the decoded URL.Path here, including when a slash or
	// a reserved filename character was percent-encoded in the request.
	if privateUIUpdaterPath(name) {
		return nil, os.ErrNotExist
	}
	relative := filepath.FromSlash(path.Clean("/" + name)[1:])
	resolved, err := filepath.EvalSymlinks(filepath.Join(s.root, relative))
	if err != nil {
		return nil, err
	}
	// Check the actual target as well, covering symlink aliases and Windows
	// short (8.3) path names without disabling normal UI symlinks.
	if privateUIUpdaterPath(resolved) {
		return nil, os.ErrNotExist
	}
	file, err := http.Dir(s.root).Open(name)
	if err != nil {
		return nil, err
	}
	return uiFile{File: file}, nil
}

type uiFile struct{ http.File }

func (f uiFile) Readdir(count int) ([]fs.FileInfo, error) {
	var visible []fs.FileInfo
	for {
		limit := count
		if count > 0 {
			limit -= len(visible)
		}
		entries, err := f.File.Readdir(limit)
		if visible == nil {
			visible = entries[:0]
		}
		for _, entry := range entries {
			if !privateUIUpdaterPath(entry.Name()) {
				visible = append(visible, entry)
			}
		}
		if err != nil || count <= 0 || len(visible) >= count {
			return visible, err
		}
	}
}

// Preserve FileServer's ReadDir fast path, which avoids statting every entry
// just to display an ordinary directory listing.
func (f uiFile) ReadDir(count int) ([]fs.DirEntry, error) {
	reader, ok := f.File.(fs.ReadDirFile)
	if !ok {
		entries, err := f.Readdir(count)
		visible := make([]fs.DirEntry, len(entries))
		for i, entry := range entries {
			visible[i] = fs.FileInfoToDirEntry(entry)
		}
		return visible, err
	}
	var visible []fs.DirEntry
	for {
		limit := count
		if count > 0 {
			limit -= len(visible)
		}
		entries, err := reader.ReadDir(limit)
		if visible == nil {
			visible = entries[:0]
		}
		for _, entry := range entries {
			if !privateUIUpdaterPath(entry.Name()) {
				visible = append(visible, entry)
			}
		}
		if err != nil || count <= 0 || len(visible) >= count {
			return visible, err
		}
	}
}
