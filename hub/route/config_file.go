package route

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"go.yaml.in/yaml/v3"
)

const (
	configFileMaxSize     = 4 << 20
	configFileMaxBodySize = 6*configFileMaxSize + 1024 // A JSON escape can use six bytes per input byte.
	configFileMaxNodes    = 100000
	configFileMaxDepth    = 64
)

// Serializing before reading/decoding also bounds concurrent configuration
// buffers. Busy callers retry instead of queuing large request bodies in RAM.
var configFileMu sync.Mutex

type configFileDocument struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Revision string `json:"revision"`
	MaxSize  int    `json:"max_size"`
}

type configFileFailure struct {
	status  int
	message string
}

var (
	configFileConflict    = &configFileFailure{http.StatusConflict, "Configuration file changed; reload it before saving"}
	configFileReadError   = &configFileFailure{http.StatusInternalServerError, "Cannot read the current configuration file"}
	configFileSaveError   = &configFileFailure{http.StatusInternalServerError, "Cannot safely save the configuration file; the original file was preserved"}
	configFileTooLarge    = &configFileFailure{http.StatusRequestEntityTooLarge, "Configuration file exceeds the 4 MiB limit"}
	configFileInvalid     = &configFileFailure{http.StatusBadRequest, "Configuration must be a nonempty, single YAML mapping with valid configuration field types"}
	configFileUnsupported = &configFileFailure{http.StatusUnprocessableEntity, "Editing requires a regular configuration file without symbolic links in its path"}
)

type configPathPart struct {
	path string
	info os.FileInfo
}

type configFileSnapshot struct {
	document configFileDocument
	info     os.FileInfo
	metadata configFileMetadata
	parts    []configPathPart
}

// This runs before controller authentication so errors and unsupported methods
// cannot be cached either. Authentication itself remains the existing policy.
func configFileNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/configs/file" || strings.HasPrefix(r.URL.Path, "/configs/file/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func configFileRespondError(w http.ResponseWriter, r *http.Request, failure *configFileFailure) {
	render.Status(r, failure.status)
	render.JSON(w, r, newError(failure.message))
}

func configFileBegin(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if !configFileMu.TryLock() {
		w.Header().Set("Retry-After", "1")
		configFileRespondError(w, r, &configFileFailure{http.StatusServiceUnavailable, "Configuration file is busy; retry shortly"})
		return false
	}
	return true
}

func getConfigFile(w http.ResponseWriter, r *http.Request) {
	if !configFileBegin(w, r) {
		return
	}
	defer configFileMu.Unlock()
	snapshot, failure := readCurrentConfigFile()
	if failure != nil {
		configFileRespondError(w, r, failure)
		return
	}
	render.JSON(w, r, snapshot.document)
}

func putConfigFile(w http.ResponseWriter, r *http.Request) {
	if !configFileBegin(w, r) {
		return
	}
	defer configFileMu.Unlock()
	if r.ContentLength > configFileMaxBodySize {
		configFileRespondError(w, r, configFileTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, configFileMaxBodySize)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		Content  *string `json:"content"`
		Revision *string `json:"revision"`
	}
	if err := decoder.Decode(&request); err != nil {
		configFileDecodeError(w, r, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		configFileDecodeError(w, r, err)
		return
	}
	if request.Content == nil || request.Revision == nil || len(*request.Revision) != sha256.Size*2 {
		configFileRespondError(w, r, &configFileFailure{http.StatusBadRequest, "Both content and the current revision are required"})
		return
	}
	if len(*request.Content) > configFileMaxSize {
		configFileRespondError(w, r, configFileTooLarge)
		return
	}
	if failure := validateConfigFile([]byte(*request.Content)); failure != nil {
		configFileRespondError(w, r, failure)
		return
	}
	snapshot, failure := readCurrentConfigFile()
	if failure != nil {
		configFileRespondError(w, r, failure)
		return
	}
	if !safeEqual(*request.Revision, snapshot.document.Revision) {
		configFileRespondError(w, r, configFileConflict)
		return
	}
	if *request.Content == snapshot.document.Content {
		render.JSON(w, r, snapshot.document)
		return
	}
	document, failure := saveConfigFile(snapshot, *request.Content)
	if failure != nil {
		configFileRespondError(w, r, failure)
		return
	}
	render.JSON(w, r, document)
}

func configFileDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		configFileRespondError(w, r, configFileTooLarge)
	} else {
		configFileRespondError(w, r, &configFileFailure{http.StatusBadRequest, "Invalid configuration save request"})
	}
}

func configFileText(data []byte) *configFileFailure {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))
	if bytes.HasPrefix(trimmed, []byte("-----BEGIN AGE ENCRYPTED FILE-----")) || bytes.HasPrefix(trimmed, []byte("age-encryption.org/v1")) {
		return &configFileFailure{http.StatusUnsupportedMediaType, "Encrypted configuration files cannot be edited in the dashboard"}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return &configFileFailure{http.StatusUnsupportedMediaType, "Configuration must be UTF-8 text"}
	}
	return nil
}

func validateConfigFile(data []byte) *configFileFailure {
	if failure := configFileText(data); failure != nil {
		return failure
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if decoder.Decode(&document) != nil || len(document.Content) != 1 {
		return configFileInvalid
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode || len(root.Content) == 0 {
		return configFileInvalid
	}
	if decoder.Decode(new(yaml.Node)) != io.EOF {
		return configFileInvalid
	}
	// Bound the subsequent typed decode, including inputs in otherwise ignored
	// extension fields. YAML's decoder separately guards alias expansion.
	type entry struct {
		node  *yaml.Node
		depth int
	}
	stack := []entry{{root, 0}}
	count := 0
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		count++
		if count > configFileMaxNodes || current.depth > configFileMaxDepth {
			return configFileInvalid
		}
		if current.node.Kind == yaml.MappingNode {
			seen := make(map[string]struct{}, len(current.node.Content)/2)
			for i := 0; i < len(current.node.Content); i += 2 {
				key := current.node.Content[i]
				if key.Kind != yaml.ScalarNode || (key.Tag != "!!str" && key.Tag != "!!merge") {
					return configFileInvalid
				}
				if _, exists := seen[key.Value]; exists {
					return configFileInvalid
				}
				seen[key.Value] = struct{}{}
			}
		}
		for _, child := range current.node.Content {
			stack = append(stack, entry{child, current.depth + 1})
		}
	}
	// Deliberately do not call ParseRawConfig / executor.Parse here: they change
	// process globals and may download resources. Saving only checks syntax and
	// basic typed fields; the existing Reload action performs full validation.
	var raw config.RawConfig
	if root.Decode(&raw) != nil {
		return configFileInvalid
	}
	return nil
}

func inspectConfigFilePath(path string) ([]configPathPart, *configFileFailure) {
	var parts []configPathPart
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, configFileReadError
		}
		if info.Mode()&os.ModeSymlink != 0 || (current == path && !info.Mode().IsRegular()) || (current != path && !info.IsDir()) {
			return nil, configFileUnsupported
		}
		parts = append(parts, configPathPart{current, info})
		if filepath.Dir(current) == current {
			return parts, nil
		}
	}
}

func configFilePathUnchanged(parts []configPathPart) bool {
	for _, part := range parts {
		info, err := os.Lstat(part.path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(part.info, info) {
			return false
		}
	}
	return true
}

func configFileRevision(path, identity, content string, info os.FileInfo) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%d\x00", path, identity, info.Mode(), info.ModTime().UnixNano())
	io.WriteString(hash, content)
	return hex.EncodeToString(hash.Sum(nil))
}

func readCurrentConfigFile() (*configFileSnapshot, *configFileFailure) {
	path, err := filepath.Abs(C.Path.Config())
	if err != nil {
		return nil, configFileReadError
	}
	parts, failure := inspectConfigFilePath(path)
	if failure != nil {
		return nil, failure
	}
	file, err := openConfigFile(path)
	if err != nil {
		return nil, configFileReadError
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(parts[0].info, info) {
		return nil, configFileConflict
	}
	if info.Size() > configFileMaxSize {
		return nil, configFileTooLarge
	}
	metadata, err := readConfigFileMetadata(file, info)
	if err != nil {
		return nil, configFileReadError
	}
	data, err := io.ReadAll(io.LimitReader(file, configFileMaxSize+1))
	if err != nil {
		return nil, configFileReadError
	}
	if len(data) > configFileMaxSize {
		return nil, configFileTooLarge
	}
	finalInfo, err := file.Stat()
	if err != nil || int64(len(data)) != info.Size() || info.Size() != finalInfo.Size() || info.ModTime() != finalInfo.ModTime() || info.Mode() != finalInfo.Mode() || !configFilePathUnchanged(parts) {
		return nil, configFileConflict
	}
	finalMetadata, err := readConfigFileMetadata(file, finalInfo)
	if err != nil || finalMetadata.identity != metadata.identity {
		return nil, configFileConflict
	}
	currentPath, err := filepath.Abs(C.Path.Config())
	if err != nil || currentPath != path {
		return nil, configFileConflict
	}
	if failure := configFileText(data); failure != nil {
		return nil, failure
	}
	content := string(data)
	return &configFileSnapshot{
		document: configFileDocument{path, content, configFileRevision(path, metadata.identity, content, info), configFileMaxSize},
		info:     info, metadata: metadata, parts: parts,
	}, nil
}

func saveConfigFile(snapshot *configFileSnapshot, content string) (*configFileDocument, *configFileFailure) {
	directory := filepath.Dir(snapshot.document.Path)
	temporary, err := os.CreateTemp(directory, ".mihomo-config-*")
	if err != nil {
		return nil, configFileSaveError
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	// Tighten inherited access controls before putting secrets in the temporary
	// file (in particular, Windows may inherit a broader directory DACL).
	if err = preserveConfigFileMetadata(temporary, snapshot.info, snapshot.metadata); err != nil {
		return nil, configFileSaveError
	}
	if _, err = io.WriteString(temporary, content); err != nil {
		return nil, configFileSaveError
	}
	// Some Unix filesystems clear set-ID bits on a write.
	if err = temporary.Chmod(snapshot.info.Mode()); err != nil {
		return nil, configFileSaveError
	}
	if err = temporary.Sync(); err != nil {
		return nil, configFileSaveError
	}
	info, err := temporary.Stat()
	if err != nil {
		return nil, configFileSaveError
	}
	metadata, err := readConfigFileMetadata(temporary, info)
	if err != nil || temporary.Close() != nil {
		return nil, configFileSaveError
	}
	// Windows only guarantees the final last-write timestamp after the writer
	// is closed. Return a revision that a subsequent GET can reproduce.
	closedInfo, err := os.Stat(temporary.Name())
	if err != nil || !os.SameFile(info, closedInfo) || closedInfo.Size() != int64(len(content)) {
		return nil, configFileSaveError
	}
	info = closedInfo
	current, failure := readCurrentConfigFile()
	if failure != nil || current.document.Revision != snapshot.document.Revision || !configFilePathUnchanged(snapshot.parts) {
		return nil, configFileConflict
	}
	// The mutex provides compare-and-swap between editor requests. The second
	// read detects external edits and path replacements before this commit. An
	// external writer not sharing this lock can still race the final rename;
	// portable filesystems do not expose a conditional replace-by-file-ID API.
	if err = os.Rename(temporary.Name(), snapshot.document.Path); err != nil {
		return nil, configFileSaveError
	}
	// The file itself was synced before replacement. Directory syncing is best
	// effort on platforms/filesystems which do not support it; after rename the
	// saved bytes are committed, so a directory sync error cannot mean "unsaved".
	if parent, err := os.Open(directory); err == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return &configFileDocument{
		Path: snapshot.document.Path, Content: content,
		Revision: configFileRevision(snapshot.document.Path, metadata.identity, content, info), MaxSize: configFileMaxSize,
	}, nil
}
