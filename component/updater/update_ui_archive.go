package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/metacubex/mihomo/log"
)

const (
	maxUIFiles        = 10000
	maxUIFileSize     = 32 * 1024 * 1024
	maxUIExpanded     = 256 * 1024 * 1024
	maxUIPath         = 1024
	maxUIPathDepth    = 64
	uiRetainedFile    = ".mihomo-ui-retained.json"
	maxRetainedRecord = 2 * 1024 * 1024
)

type uiFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type uiContextReader struct {
	ctx context.Context
	io.Reader
}

func (r uiContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func uiArchiveName(name string) (string, error) {
	if len(name) > maxUIPath || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
		return "", errors.New("unsafe archive path")
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return "", errors.New("archive path traversal")
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." || !filepath.IsLocal(filepath.FromSlash(cleaned)) {
		return "", errors.New("invalid archive path")
	}
	return cleaned, nil
}

type uiExtraction struct {
	ctx      context.Context
	root     string
	files    map[string]bool
	names    map[string]string
	explicit map[string]bool
	total    int64
}

func (x *uiExtraction) entry(name string, directory bool, size int64, data io.Reader) error {
	if err := x.ctx.Err(); err != nil {
		return err
	}
	// tar -C dist -czf ui.tgz . emits this harmless root directory entry.
	if directory && (name == "." || name == "./") {
		return nil
	}
	name, err := uiArchiveName(name)
	if err != nil {
		return err
	}
	if path.Base(name) == uiRetainedFile {
		return errors.New("UI archive uses a reserved updater filename")
	}
	components := strings.Split(name, "/")
	if len(components) > maxUIPathDepth {
		return errors.New("UI archive path is too deep")
	}
	if x.names == nil {
		x.names, x.explicit = make(map[string]string), make(map[string]bool)
	}
	key := strings.ToLower(name)
	if x.explicit[key] {
		return errors.New("duplicate archive entry")
	}
	x.explicit[key] = true
	// Count implicit parents before creating any directory. Otherwise a small
	// archive of deeply nested empty files could exhaust filesystem inodes.
	for i := 1; i <= len(components); i++ {
		node := strings.Join(components[:i], "/")
		folded := strings.ToLower(node)
		isDirectory := i < len(components) || directory
		if previous, exists := x.names[folded]; exists {
			if previous != node || x.files[folded] != isDirectory {
				return errors.New("conflicting UI archive paths")
			}
			continue
		}
		if len(x.files) >= maxUIFiles {
			return errors.New("too many UI archive files or directories")
		}
		x.names[folded], x.files[folded] = node, isDirectory
	}
	dest := filepath.Join(x.root, filepath.FromSlash(name))
	if directory {
		return os.MkdirAll(dest, 0o755)
	}
	if size < 0 || size > maxUIFileSize || x.total+size > maxUIExpanded {
		return errors.New("expanded UI archive is too large")
	}
	x.total += size
	if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(file, io.LimitReader(uiContextReader{x.ctx, data}, size+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if n != size {
		return errors.New("UI archive entry size mismatch")
	}
	return closeErr
}

// Check the central-directory record count before archive/zip allocates its
// complete file list. ZIP64 is unnecessary inside our size/count limits.
func uiZIPReader(file *os.File, size int64) (*zip.Reader, error) {
	tailSize := int64(65557)
	if size < tailSize {
		tailSize = size
	}
	tail := make([]byte, tailSize)
	if _, err := file.ReadAt(tail, size-tailSize); err != nil {
		return nil, err
	}
	end := bytes.LastIndex(tail, []byte{'P', 'K', 5, 6})
	if end < 0 || end+22 > len(tail) || end+22+int(binary.LittleEndian.Uint16(tail[end+20:])) != len(tail) {
		return nil, errors.New("invalid ZIP directory")
	}
	record := tail[end:]
	count := int(binary.LittleEndian.Uint16(record[10:]))
	directorySize := int64(binary.LittleEndian.Uint32(record[12:]))
	offset := int64(binary.LittleEndian.Uint32(record[16:]))
	if binary.LittleEndian.Uint32(record[4:]) != 0 || int(binary.LittleEndian.Uint16(record[8:])) != count ||
		count > maxUIFiles || directorySize > maxUIArchive || offset+directorySize > size-tailSize+int64(end) {
		return nil, errors.New("unsupported or oversized ZIP directory")
	}
	reader := io.NewSectionReader(file, offset, directorySize)
	var header [46]byte
	actual := 0
	for {
		_, err := io.ReadFull(reader, header[:])
		if err == io.EOF {
			break
		}
		if err != nil || binary.LittleEndian.Uint32(header[:]) != 0x02014b50 {
			return nil, errors.New("invalid ZIP directory record")
		}
		actual++
		nameSize := int64(binary.LittleEndian.Uint16(header[28:]))
		extraSize := int64(binary.LittleEndian.Uint16(header[30:])) + int64(binary.LittleEndian.Uint16(header[32:]))
		if actual > maxUIFiles || nameSize > maxUIPath || binary.LittleEndian.Uint32(header[24:]) > maxUIFileSize {
			return nil, errors.New("ZIP file count, path, or entry exceeds its limit")
		}
		if _, err = io.CopyN(io.Discard, reader, nameSize+extraSize); err != nil {
			return nil, err
		}
	}
	if actual != count {
		return nil, errors.New("ZIP directory count mismatch")
	}
	return zip.NewReader(file, size)
}

func extractUIArchive(ctx context.Context, archivePath, dest string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	var magic [4]byte
	if _, err = io.ReadFull(file, magic[:]); err != nil {
		return "", err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if err = os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	x := uiExtraction{ctx: ctx, root: dest, files: make(map[string]bool)}
	switch {
	case bytes.Equal(magic[:], []byte{'P', 'K', 3, 4}):
		reader, err := uiZIPReader(file, info.Size())
		if err != nil {
			return "", err
		}
		for _, entry := range reader.File {
			mode := entry.Mode()
			if !mode.IsRegular() && !mode.IsDir() {
				return "", errors.New("UI archive contains a link or special file")
			}
			contents, err := entry.Open()
			if err != nil {
				return "", err
			}
			err = x.entry(entry.Name, mode.IsDir(), int64(entry.UncompressedSize64), contents)
			closeErr := contents.Close()
			if err != nil {
				return "", err
			}
			if closeErr != nil {
				return "", closeErr
			}
		}
	case magic[0] == 0x1f && magic[1] == 0x8b:
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return "", err
		}
		defer compressed.Close()
		limited := &io.LimitedReader{R: uiContextReader{ctx, compressed}, N: maxUIExpanded + maxUIFiles*2048 + 1}
		reader := tar.NewReader(limited)
		for {
			entry, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
			if entry.Typeflag != tar.TypeReg && entry.Typeflag != tar.TypeDir {
				return "", errors.New("UI archive contains a link or special file")
			}
			if err = x.entry(entry.Name, entry.Typeflag == tar.TypeDir, entry.Size, reader); err != nil {
				return "", err
			}
		}
		if _, err = io.Copy(io.Discard, limited); err != nil {
			return "", err
		}
		if limited.N == 0 {
			return "", errors.New("expanded UI archive is too large")
		}
	default:
		return "", errors.New("unknown or unsupported archive format")
	}
	root := dest
	if err = uiRegularFile(filepath.Join(root, "index.html")); err != nil {
		entries, readErr := os.ReadDir(root)
		if readErr != nil || len(entries) != 1 || !entries[0].IsDir() {
			return "", errors.New("archive does not contain a dashboard index.html")
		}
		root = filepath.Join(root, entries[0].Name())
		if err = uiRegularFile(filepath.Join(root, "index.html")); err != nil {
			return "", errors.New("archive does not contain a dashboard index.html")
		}
	}
	return root, nil
}

func uiRegularFile(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxUIFileSize {
		return errors.New("invalid dashboard file")
	}
	return nil
}

func uiFileChecksum(ctx context.Context, name string) (uiFile, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return uiFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxUIFileSize {
		return uiFile{}, errors.New("UI contains a link, special file, or oversized asset")
	}
	file, err := os.Open(name)
	if err != nil {
		return uiFile{}, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(uiContextReader{ctx, file}, maxUIFileSize+1))
	if err != nil {
		return uiFile{}, err
	}
	if n > maxUIFileSize {
		return uiFile{}, errors.New("UI asset is too large")
	}
	return uiFile{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: n}, nil
}

func scanUITree(ctx context.Context, root string, ignoreRetained bool) (string, map[string]uiFile, error) {
	files := make(map[string]uiFile)
	retained := map[string]uiFile{}
	if ignoreRetained {
		if data, err := readUIFile(filepath.Join(root, uiRetainedFile), maxRetainedRecord); err == nil {
			_ = json.Unmarshal(data, &retained)
		}
	}
	var total int64
	var count int
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maxUIFiles*2+1 {
			return errors.New("installed dashboard has too many files")
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !entry.Type().IsRegular() {
			return errors.New("UI contains a link or special file")
		}
		if ignoreRetained && rel == uiRetainedFile {
			return nil
		}
		actual, err := uiFileChecksum(ctx, name)
		if err != nil {
			return err
		}
		total += actual.Size
		if total > maxUIExpanded*2 {
			return errors.New("installed dashboard exceeds the size limit")
		}
		if previous, ok := retained[rel]; ignoreRetained && ok && uiStaticAsset(rel) && previous == actual {
			return nil
		}
		files[rel] = actual
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		_, _ = io.WriteString(hash, name+"\x00"+files[name].SHA256+"\n")
	}
	return hex.EncodeToString(hash.Sum(nil)), files, nil
}

func uiStaticAsset(name string) bool {
	for _, prefix := range []string{"assets/", "static/", "js/", "css/", "_next/static/"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".js", ".css", ".wasm", ".woff", ".woff2", ".ttf", ".otf", ".svg", ".png", ".jpg", ".jpeg", ".webp", ".avif", ".ico":
		return true
	}
	return false
}

// Keep the previous generation's assets for open tabs. The bounded marker only
// contains public asset names/digests, never URLs, credentials, or DNS data.
// It excludes retained files from later comparisons and prevents accumulation.
func retainUIAssets(ctx context.Context, current, staged string, oldFiles, newFiles map[string]uiFile) error {
	retained := make(map[string]uiFile)
	var total int64
	for name, previous := range oldFiles {
		if !uiStaticAsset(name) {
			continue
		}
		if _, exists := newFiles[name]; exists {
			continue
		}
		if cleaned, err := uiArchiveName(name); err != nil || cleaned != name {
			return errors.New("invalid previous UI asset path")
		}
		total += previous.Size
		if total > maxUIExpanded || len(retained) >= maxUIFiles {
			return errors.New("previous UI assets exceed the retention limit")
		}
		source := filepath.Join(current, filepath.FromSlash(name))
		destination := filepath.Join(staged, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := copyUIAsset(ctx, source, destination, previous); err != nil {
			return err
		}
		retained[name] = previous
	}
	if len(retained) == 0 {
		return nil
	}
	data, err := json.Marshal(retained)
	if err != nil {
		return err
	}
	if len(data) > maxRetainedRecord {
		return errors.New("retained UI asset metadata is too large")
	}
	return os.WriteFile(filepath.Join(staged, uiRetainedFile), data, 0o644)
}

func copyUIAsset(ctx context.Context, source, destination string, expected uiFile) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("previous UI asset changed during update")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(uiContextReader{ctx, in}, maxUIFileSize+1))
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if n != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return errors.New("previous UI asset changed during update")
	}
	return closeErr
}

func installUIDirectory(staged, target string, rename func(string, string) error, removeAll func(string) error) error {
	// At most one recovery directory exists for this target, including across
	// restarts. Never remove an unknown directory or the only surviving UI.
	id := sha256.Sum256([]byte(target))
	marker := "mihomo-ui-backup-v1\n" + hex.EncodeToString(id[:]) + "\n"
	backupRoot := filepath.Join(filepath.Dir(target), ".mihomo-ui-backup-"+hex.EncodeToString(id[:16]))
	backup := filepath.Join(backupRoot, "previous")
	if info, err := os.Lstat(backupRoot); err == nil {
		data, readErr := readUIFile(filepath.Join(backupRoot, "owner"), 256)
		if !info.IsDir() || readErr != nil || string(data) != marker {
			return errors.New("UI recovery directory is not owned by this updater")
		}
		if _, targetErr := os.Lstat(target); os.IsNotExist(targetErr) {
			if _, oldErr := os.Lstat(backup); oldErr == nil {
				if restoreErr := rename(backup, target); restoreErr != nil {
					return errors.New("previous dashboard recovery is pending")
				}
			}
		} else if targetErr != nil {
			return targetErr
		}
		if err = removeAll(backupRoot); err != nil {
			return errors.New("previous dashboard cleanup is pending")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return rename(staged, target)
	} else if err != nil {
		return err
	}
	if err := os.Mkdir(backupRoot, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(backupRoot, "owner"), []byte(marker), 0o600); err != nil {
		_ = removeAll(backupRoot)
		return err
	}
	if err := rename(target, backup); err != nil {
		_ = removeAll(backupRoot)
		return fmt.Errorf("preserve existing dashboard: %w", err)
	}
	if err := rename(staged, target); err != nil {
		if restoreErr := rename(backup, target); restoreErr != nil {
			return fmt.Errorf("install dashboard: %v; restore failed: %v; previous UI retained in recovery directory", err, restoreErr)
		}
		_ = removeAll(backupRoot)
		return fmt.Errorf("install dashboard (previous version restored): %w", err)
	}
	if err := removeAll(backupRoot); err != nil {
		log.Warnln("UI updated successfully, but its previous directory could not be removed; cleanup will be retried before the next update")
	}
	return nil
}
