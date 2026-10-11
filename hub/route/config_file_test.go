package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptest"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
)

func configFileFixture(t *testing.T, content string) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatal(err)
	}
	oldPath, oldEmbed := C.Path.Config(), embedMode
	C.SetConfig(path)
	embedMode = false
	t.Cleanup(func() {
		C.SetConfig(oldPath)
		embedMode = oldEmbed
	})
	return path
}

func requestConfigFile(t *testing.T, method string, body any) (*httptest.ResponseRecorder, configFileDocument) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	response := httptest.NewRecorder()
	configRouter().ServeHTTP(response, httptest.NewRequest(method, "/file", reader))
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("config document response was cacheable")
	}
	var document configFileDocument
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
	}
	return response, document
}

func saveConfigRequest(content, revision string) map[string]string {
	return map[string]string{"content": content, "revision": revision}
}

func assertConfigBytes(t *testing.T, path, content string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("configuration contents changed unexpectedly: read error=%v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".mihomo-config-*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("configuration edit left a temporary file behind")
	}
}

func TestConfigFileReadSaveAndRevision(t *testing.T) {
	original := "# retain comments\nmode: rule\nsecret: private-value\n"
	path := configFileFixture(t, original)
	response, initial := requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusOK || initial.Content != original || initial.Path != path || initial.MaxSize != configFileMaxSize || len(initial.Revision) != 64 || !initial.Writable {
		t.Fatalf("unexpected initial document: status=%d", response.Code)
	}
	_, same := requestConfigFile(t, http.MethodGet, nil)
	if same.Revision != initial.Revision {
		t.Fatal("unchanged file revision was unstable")
	}
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := "# replacement preserves formatting\nmode: direct\nsecret: private-value\n"
	response, saved := requestConfigFile(t, http.MethodPut, saveConfigRequest(replacement, initial.Revision))
	if response.Code != http.StatusOK || saved.Content != replacement || saved.Revision == initial.Revision || !saved.Writable {
		t.Fatalf("save failed: status=%d body=%s", response.Code, response.Body.String())
	}
	assertConfigBytes(t, path, replacement)
	newInfo, err := os.Stat(path)
	if err != nil || newInfo.Mode() != oldInfo.Mode() {
		t.Fatal("save did not preserve file permissions")
	}
	_, reread := requestConfigFile(t, http.MethodGet, nil)
	if reread.Revision != saved.Revision {
		t.Fatal("save response revision differs from the saved file")
	}
	response, _ = requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: rule\n", initial.Revision))
	if response.Code != http.StatusConflict {
		t.Fatal("stale editor overwrote the latest save")
	}
	assertConfigBytes(t, path, replacement)
	response, unchanged := requestConfigFile(t, http.MethodPut, saveConfigRequest(replacement, saved.Revision))
	if response.Code != http.StatusOK || unchanged.Revision != saved.Revision {
		t.Fatal("no-op save changed the file revision")
	}
}

func TestConfigFileRejectsExternalContentIdentityAndPathChanges(t *testing.T) {
	for _, change := range []string{"content", "identity", "path"} {
		t.Run(change, func(t *testing.T) {
			original := "mode: rule\n"
			path := configFileFixture(t, original)
			_, initial := requestConfigFile(t, http.MethodGet, nil)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := original
			switch change {
			case "content":
				expected = "mode: direct\n"
				if err := os.WriteFile(path, []byte(expected), info.Mode()); err != nil {
					t.Fatal(err)
				}
			case "identity":
				newPath := path + ".new"
				if err := os.WriteFile(newPath, []byte(original), info.Mode()); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(newPath, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(newPath, path); err != nil {
					t.Fatal(err)
				}
			case "path":
				other := path + ".other"
				if err := os.WriteFile(other, []byte(original), info.Mode()); err != nil {
					t.Fatal(err)
				}
				C.SetConfig(other)
			}
			response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest("mode: global\n", initial.Revision))
			if response.Code != http.StatusConflict {
				t.Fatalf("external %s change was not rejected: %d", change, response.Code)
			}
			assertConfigBytes(t, path, expected)
		})
	}
}

func TestConfigFileValidationLeavesOriginalIntactAndHidesSecrets(t *testing.T) {
	original := "mode: rule\n"
	path := configFileFixture(t, original)
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	for _, content := range []string{
		"", "# just a comment", "{}", "[]", "null", "- mode: rule", "mode: [", "mode: rule\n---\nmode: global\n",
		"mode: rule\n---\n", "mixed-port: private-secret\n", "log-level: private-secret\n", "mode: private-secret\n",
		"mode: rule\nmode: global\n", "extension: {key: value, key: private-secret}\n", "? [a, b]\n: private-secret\n",
		"mode: rule\nsecret: \x00private-secret\n", "-----BEGIN AGE ENCRYPTED FILE-----\nprivate-secret",
	} {
		response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest(content, initial.Revision))
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("invalid YAML accepted: status=%d", response.Code)
		}
		if strings.Contains(response.Body.String(), "private-secret") {
			t.Fatal("validation error disclosed a config value")
		}
		assertConfigBytes(t, path, original)
	}
	for _, body := range []map[string]string{
		{"content": "mode: direct\n"},
		{"revision": initial.Revision},
		{"content": "mode: direct\n", "revision": initial.Revision, "path": "/etc/unrelated.yaml"},
	} {
		response, _ := requestConfigFile(t, http.MethodPut, body)
		if response.Code != http.StatusBadRequest {
			t.Fatal("malformed save request accepted")
		}
		assertConfigBytes(t, path, original)
	}
	valid := "shared: &shared\n  type: select\nproxy-groups:\n  - <<: *shared\n    name: Choice\n    proxies: [DIRECT]\nmode: rule\n"
	response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest(valid, initial.Revision))
	if response.Code != http.StatusOK {
		t.Fatalf("ordinary YAML anchors/merges rejected: %d", response.Code)
	}
}

func TestConfigFileSizeAndBusyBounds(t *testing.T) {
	original := "mode: rule\n"
	path := configFileFixture(t, original)
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest("secret: "+strings.Repeat("x", configFileMaxSize), initial.Revision))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized decoded contents accepted: %d", response.Code)
	}
	// Unknown-length bodies must hit the byte cap too, including trailing JSON
	// whitespace after a small valid request.
	data, err := json.Marshal(saveConfigRequest(original, initial.Revision))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/file", io.MultiReader(bytes.NewReader(data), strings.NewReader(strings.Repeat(" ", configFileMaxBodySize))))
	request.ContentLength = -1
	response = httptest.NewRecorder()
	configRouter().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized streaming body accepted: %d", response.Code)
	}
	assertConfigBytes(t, path, original)
	configFileMu.Lock()
	response = httptest.NewRecorder()
	getConfigFile(response, httptest.NewRequest(http.MethodGet, "/file", nil))
	configFileMu.Unlock()
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatal("busy editor did not bound concurrent requests")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), configFileMaxSize+1), 0640); err != nil {
		t.Fatal(err)
	}
	response, _ = requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatal("oversized on-disk configuration was read")
	}
}

func TestConfigFileRejectsUnsupportedDiskFormatsAndPaths(t *testing.T) {
	path := configFileFixture(t, "mode: rule\n")
	for _, content := range [][]byte{
		[]byte("-----BEGIN AGE ENCRYPTED FILE-----\nprivate-secret"),
		[]byte("age-encryption.org/v1\nprivate-secret"),
		{0xff, 0xfe, 0xfd},
	} {
		if err := os.WriteFile(path, content, 0640); err != nil {
			t.Fatal(err)
		}
		response, _ := requestConfigFile(t, http.MethodGet, nil)
		if response.Code != http.StatusUnsupportedMediaType || strings.Contains(response.Body.String(), "private-secret") {
			t.Fatal("unsupported config data was exposed")
		}
	}
	C.SetConfig(filepath.Dir(path))
	response, _ := requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatal("directory config was not rejected")
	}
	if runtime.GOOS == "windows" {
		return // Symlink creation requires additional privileges on Windows.
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	C.SetConfig(link)
	response, _ = requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatal("symlink config was not rejected")
	}
	directoryLink := filepath.Join(filepath.Dir(path), "linked-directory")
	if err := os.Symlink(filepath.Dir(path), directoryLink); err != nil {
		t.Fatal(err)
	}
	C.SetConfig(filepath.Join(directoryLink, "config.yaml"))
	response, _ = requestConfigFile(t, http.MethodGet, nil)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatal("configuration path traversed a symlink directory")
	}
}

func TestConfigFileSecondCheckDoesNotOverwriteChangedFile(t *testing.T) {
	original := "mode: rule\n"
	path := configFileFixture(t, original)
	snapshot, failure := readCurrentConfigFile()
	if failure != nil {
		t.Fatal(failure.message)
	}
	outside := "mode: direct\n"
	if err := os.WriteFile(path, []byte(outside), 0640); err != nil {
		t.Fatal(err)
	}
	_, failure = saveConfigFile(snapshot, "mode: global\n")
	if failure != configFileConflict {
		t.Fatal("write commit failed to recheck concurrent external edit")
	}
	assertConfigBytes(t, path, outside)
}

func TestConfigFileConcurrentSavesHaveOneWinner(t *testing.T) {
	path := configFileFixture(t, "mode: rule\n")
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	type result struct {
		status  int
		content string
	}
	const writers = 8
	results := make(chan result, writers)
	start := make(chan struct{})
	handler := configRouter()
	for i := 0; i < writers; i++ {
		content := fmt.Sprintf("mode: rule\n# writer %d\n", i)
		body, err := json.Marshal(saveConfigRequest(content, initial.Revision))
		if err != nil {
			t.Fatal(err)
		}
		go func(body []byte, content string) {
			<-start
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/file", bytes.NewReader(body)))
			results <- result{response.Code, content}
		}(body, content)
	}
	close(start)
	winners, saved := 0, ""
	for i := 0; i < writers; i++ {
		result := <-results
		switch result.status {
		case http.StatusOK:
			winners++
			saved = result.content
		case http.StatusConflict, http.StatusServiceUnavailable:
		default:
			t.Fatalf("unexpected concurrent save status: %d", result.status)
		}
	}
	if winners != 1 {
		t.Fatalf("expected one atomic save, got %d", winners)
	}
	assertConfigBytes(t, path, saved)
}

type configFileUntouchedResolver struct {
	resolver.Resolver
	cleared atomic.Bool
}

func (r *configFileUntouchedResolver) ClearCache() { r.cleared.Store(true) }

func TestConfigFileSaveDoesNotApplyOrClearDNS(t *testing.T) {
	configFileFixture(t, "mode: rule\n")
	oldMode, oldLevel, oldIPv6 := tunnel.Mode(), log.Level(), resolver.DisableIPv6
	oldDNS, oldDefault, oldDirect, oldProxy := tunnel.DNSRuleRoutingEnabled(), resolver.DefaultResolver, resolver.DirectHostResolver, resolver.ProxyServerHostResolver
	stub := &configFileUntouchedResolver{}
	resolver.DefaultResolver = stub
	t.Cleanup(func() {
		tunnel.SetMode(oldMode)
		log.SetLevel(oldLevel)
		resolver.DisableIPv6 = oldIPv6
		tunnel.SetDNSRuleRouting(oldDNS)
		resolver.DefaultResolver = oldDefault
		resolver.DirectHostResolver = oldDirect
		resolver.ProxyServerHostResolver = oldProxy
	})
	_, initial := requestConfigFile(t, http.MethodGet, nil)
	// Use the inverse effective IPv6 value so even the parsing stage (without
	// ApplyConfig) would be caught if it mutated global resolver settings.
	content := fmt.Sprintf("mode: global\nlog-level: debug\nipv6: %t\ndns-rule-routing: true\nproxy-providers:\n  never-fetch:\n    type: http\n    url: https://invalid.invalid/never-fetch\n", oldIPv6)
	response, _ := requestConfigFile(t, http.MethodPut, saveConfigRequest(content, initial.Revision))
	if response.Code != http.StatusOK {
		t.Fatalf("save failed: %d", response.Code)
	}
	if tunnel.Mode() != oldMode || log.Level() != oldLevel || resolver.DisableIPv6 != oldIPv6 || tunnel.DNSRuleRoutingEnabled() != oldDNS || resolver.DefaultResolver != stub || resolver.DirectHostResolver != oldDirect || resolver.ProxyServerHostResolver != oldProxy || stub.cleared.Load() {
		t.Fatal("saving the configuration changed running settings or DNS state")
	}
}

func TestConfigFileControllerAuthenticationAndEmbedMode(t *testing.T) {
	configFileFixture(t, "mode: rule\nsecret: private-secret\n")
	handler := router(false, "controller-secret", "", Cors{})
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		request := httptest.NewRequest(method, "/configs/file", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "private-secret") {
			t.Fatal("config file route bypassed controller authentication or was cacheable")
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/configs/file", nil)
	request.Header.Set("Authorization", "Bearer controller-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatal("authenticated config file request failed")
	}
	embedMode = true
	handler = router(false, "controller-secret", "", Cors{})
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		request := httptest.NewRequest(method, "/configs/file", nil)
		request.Header.Set("Authorization", "Bearer controller-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("embedded controller exposed configuration file access")
		}
	}
}
