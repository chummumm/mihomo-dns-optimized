package updater

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	C "github.com/metacubex/mihomo/constant"
)

func TestCoreUpdaterTargetMatrix(t *testing.T) {
	data, err := os.ReadFile("../../packaging/targets.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID, GOOS, GOARCH string
		Env              map[string]string
	}
	if err = json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 37 {
		t.Fatalf("release matrix changed: %d targets", len(rows))
	}
	for _, row := range rows {
		t.Run(row.ID, func(t *testing.T) {
			target, err := coreTarget(row.GOOS, row.GOARCH, row.Env)
			if err != nil || target != row.ID {
				t.Fatalf("update target=%q err=%v; want %s", target, err, row.ID)
			}
		})
	}
	for _, arm := range []string{"5,softfloat", "6,hardfloat", "7,hardfloat"} {
		if target, err := coreTarget("linux", "arm", map[string]string{"GOARM": arm}); err != nil || target != "linux-armv"+arm[:1] {
			t.Fatalf("recorded Go ARM ABI %s: %s %v", arm, target, err)
		}
	}
	for _, row := range []struct {
		os, arch string
		env      map[string]string
	}{
		{"linux", "amd64", map[string]string{"GOAMD64": "v4"}},
		{"linux", "arm", map[string]string{"GOARM": "7,softfloat"}},
		{"linux", "mips64", map[string]string{"GOMIPS64": "softfloat"}},
		{"windows", "386", map[string]string{"GO386": "softfloat"}},
		{"android", "amd64", map[string]string{"GOAMD64": "v3"}},
		{"plan9", "amd64", nil},
	} {
		if target, err := coreTarget(row.os, row.arch, row.env); err == nil {
			t.Fatalf("unsupported ABI silently mapped to %q", target)
		}
	}
	if !strings.HasPrefix(DefaultCoreUpdater.CoreBaseName(), "mihomo-dns-") {
		t.Fatal("updater lost fork asset prefix")
	}
}

func TestCoreUpdaterNumericVersions(t *testing.T) {
	for _, pair := range [][2]string{
		{"v1.19.32-dns-optimized-12", "v1.19.32-dns-optimized-2"},
		{"v1.19.33-dns-optimized-1", "v1.19.32-dns-optimized-999"},
		{"v2.0.0-dns-optimized-1", "v1.99.99-dns-optimized-9"},
	} {
		newer, err := parseReleaseVersion(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		older, err := parseReleaseVersion(pair[1])
		if err != nil || newer.compare(older) != 1 || older.compare(newer) != -1 || newer.compare(newer) != 0 {
			t.Fatalf("numeric ordering failed: %v", pair)
		}
	}
	for _, invalid := range []string{
		"v1.19.32", "alpha", "v1.19.32-dns.2", "v1.19.32-dns-optimized-0",
		"v1.19.32-dns-optimized-01", "v1.19.32-dns-optimized-1/../core",
		"v1.19.32-dns-optimized-1\nother", "v1.19.32-dns-optimized-18446744073709551616",
	} {
		if _, err := parseReleaseVersion(invalid); err == nil {
			t.Fatalf("accepted unsafe/non-fork release %q", invalid)
		}
	}
}

func testCoreArchive(t *testing.T, payload []byte, member string, windows bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if windows {
		writer := zip.NewWriter(&buffer)
		file, err := writer.Create(member)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		writer := gzip.NewWriter(&buffer)
		writer.Name = member
		if _, err := writer.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buffer.Bytes()
}

func TestCoreUpdaterPinnedReleaseAndFailurePreservesExecutable(t *testing.T) {
	const version = "v1.19.32-dns-optimized-12"
	oldVersion := C.Version
	C.Version = "v1.19.32-dns.2" // migration from the previous fork series
	t.Cleanup(func() { C.Version = oldVersion })
	newCore := []byte("isolated updated core fixture\n")
	if runtime.GOOS == "darwin" {
		// Exercise re-signing an already signed native Go executable. The
		// staged fixture is never run or used to replace the test process.
		path, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		newCore, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, fault := range []string{"", "checksum", "missing-checksum", "duplicate-checksum", "archive-http", "version-http", "official-version", "empty-executable", "broken-archive"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			current := filepath.Join(dir, "mihomo")
			oldCore := []byte("existing core remains recoverable\n")
			if err := os.WriteFile(current, oldCore, 0o751); err != nil {
				t.Fatal(err)
			}
			base := DefaultCoreUpdater.CoreBaseName()
			member, extension := base, ".gz"
			if runtime.GOOS == "windows" {
				member, extension = base+".exe", ".zip"
			}
			asset := base + "-" + version + extension
			payload := newCore
			if fault == "empty-executable" {
				payload = nil
			}
			archive := testCoreArchive(t, payload, member, runtime.GOOS == "windows")
			if fault == "broken-archive" {
				archive = []byte("checksum matches but this is not an archive")
			}
			sum := sha256.Sum256(archive)
			checksums := fmt.Sprintf("%x  %s\n", sum, asset)
			switch fault {
			case "checksum":
				checksums = strings.Repeat("0", 64) + "  " + asset + "\n"
			case "missing-checksum":
				checksums = fmt.Sprintf("%x  another-architecture.gz\n", sum)
			case "duplicate-checksum":
				checksums += checksums
			}
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/releases/latest/download/version.txt":
					if fault == "version-http" {
						http.Error(w, "not found", http.StatusNotFound)
					} else if fault == "official-version" {
						fmt.Fprintln(w, "v1.19.32")
					} else {
						fmt.Fprintln(w, version)
					}
				case "/releases/download/" + version + "/SHA256SUMS":
					fmt.Fprint(w, checksums)
				case "/releases/download/" + version + "/" + asset:
					if fault == "archive-http" {
						http.Error(w, "not found", http.StatusNotFound)
					} else {
						w.Write(archive)
					}
				default:
					t.Errorf("unrequested source or unpinned release URL: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			u := CoreUpdater{releaseURL: server.URL + "/releases/"}
			err := u.Update(current, ReleaseChannel, false)
			actual, readErr := os.ReadFile(current)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if fault == "" {
				if err != nil || (runtime.GOOS != "darwin" && !bytes.Equal(actual, newCore)) {
					t.Fatalf("update failed: %v; installed %d bytes, expected %d bytes", err, len(actual), len(newCore))
				}
				if runtime.GOOS == "darwin" {
					if err := exec.Command("/usr/bin/codesign", "--verify", current).Run(); err != nil {
						t.Fatalf("installed native core signature is invalid: %v", err)
					}
					if _, err := buildinfo.ReadFile(current); err != nil {
						t.Fatalf("signed executable lost its Go build information: %v", err)
					}
				}
				backup, err := os.ReadFile(filepath.Join(dir, "meta-backup", "mihomo"))
				if err != nil || !bytes.Equal(backup, oldCore) {
					t.Fatalf("backup differs: %q %v", backup, err)
				}
				if runtime.GOOS != "windows" {
					info, err := os.Stat(current)
					if err != nil || info.Mode().Perm() != 0o751 {
						t.Fatalf("executable permissions lost: %v %v", info, err)
					}
				}
				mu.Lock()
				count := len(paths)
				mu.Unlock()
				if count != 3 {
					t.Fatalf("expected one version, checksum and archive request; got %d", count)
				}
			} else if err == nil || !bytes.Equal(actual, oldCore) {
				t.Fatalf("bad release touched installed executable: error=%v contents=%q", err, actual)
			}
			matches, err := filepath.Glob(filepath.Join(dir, ".mihomo-update-*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("staging was not cleaned up: %v %v", matches, err)
			}
		})
	}
}

func TestCoreUpdaterChannelsAndDowngrade(t *testing.T) {
	oldVersion := C.Version
	C.Version = "v1.19.32-dns-optimized-12"
	t.Cleanup(func() { C.Version = oldVersion })
	current := filepath.Join(t.TempDir(), "mihomo")
	if err := os.WriteFile(current, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	latest := "v1.19.32-dns-optimized-2"
	requests := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		version := latest
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/version.txt") {
			t.Errorf("download attempted on downgrade/equal version: %s", r.URL.Path)
		}
		fmt.Fprintln(w, version)
	}))
	defer server.Close()
	u := CoreUpdater{releaseURL: server.URL + "/releases/"}
	for _, channel := range []string{AlphaChannel, "other"} {
		if err := u.Update(current, channel, false); err == nil {
			t.Fatalf("accepted unsupported channel %q", channel)
		}
	}
	mu.Lock()
	count := requests
	mu.Unlock()
	if count != 0 {
		t.Fatal("unsupported channel made a network request")
	}
	if err := u.Update(current, "auto", false); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("numeric downgrade accepted: %v", err)
	}
	mu.Lock()
	latest = C.Version
	mu.Unlock()
	if err := u.Update(current, "", false); err == nil || !strings.Contains(err.Error(), "already using latest version") {
		t.Fatalf("equal-version dashboard behavior changed: %v", err)
	}
}

func TestCoreUpdaterInstallFailureRestoresExecutable(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "mihomo")
	original := []byte("recover this executable")
	if err := os.WriteFile(current, original, 0o755); err != nil {
		t.Fatal(err)
	}
	u := &CoreUpdater{}
	// A failed final rename must preserve the Unix original and restore the
	// Windows executable after its required move to the backup directory.
	if err := u.install(current, filepath.Join(dir, "missing-staged-core")); err == nil {
		t.Fatal("expected replacement failure")
	}
	actual, err := os.ReadFile(current)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("failed replacement lost the current executable: %v %q", err, actual)
	}
}

func TestCoreUpdaterArchiveNamesAndChecksumValidation(t *testing.T) {
	dir := t.TempDir()
	u := &CoreUpdater{}
	payload := []byte("binary")
	gzipPath := filepath.Join(dir, "core.gz")
	if err := os.WriteFile(gzipPath, testCoreArchive(t, payload, "../escape", false), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "staged")
	if err := u.unpack(gzipPath, output, "staged", 0o755); err != nil {
		t.Fatal(err)
	}
	if actual, _ := os.ReadFile(output); !bytes.Equal(actual, payload) {
		t.Fatal("gzip did not use the caller-selected output path")
	}
	for _, member := range []string{"../core.exe", "other.exe", "nested/core.exe"} {
		path := filepath.Join(dir, "core.zip")
		if err := os.WriteFile(path, testCoreArchive(t, payload, member, true), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := u.unpack(path, filepath.Join(dir, "core.exe"), "core.exe", 0o755); err == nil {
			t.Fatalf("accepted unexpected archive member %q", member)
		}
	}
	sum := sha256.Sum256(payload)
	for _, invalid := range []string{
		"invalid", "abcd  core.gz", hex.EncodeToString(sum[:]) + "  absent.gz",
		hex.EncodeToString(sum[:]) + "  core.gz\n" + hex.EncodeToString(sum[:]) + "  core.gz",
	} {
		if _, err := releaseChecksum([]byte(invalid), "core.gz"); err == nil {
			t.Fatalf("accepted malformed or incomplete checksums: %q", invalid)
		}
	}
}
