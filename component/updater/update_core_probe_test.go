package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	coreProbeFixtureOnce   sync.Once
	coreProbeFixtureBinary []byte
	coreProbeFixtureError  error
)

// Use a small native program instead of shell scripts or mocked validation.
// The same fixture runs on Unix and Windows and also exercises Darwin signing.
func coreTestExecutable(t *testing.T) []byte {
	t.Helper()
	coreProbeFixtureOnce.Do(func() {
		var dir string
		dir, coreProbeFixtureError = os.MkdirTemp("", "mihomo-core-probe-test-")
		if coreProbeFixtureError != nil {
			return
		}
		defer os.RemoveAll(dir)
		suffix := ""
		if runtime.GOOS == "windows" {
			suffix = ".exe"
		}
		path := filepath.Join(dir, "core-probe"+suffix)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"+suffix), "build", "-trimpath", "-ldflags=-s -w", "-o", path, "./testdata/core_probe/main.go")
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := command.CombinedOutput(); err != nil {
			coreProbeFixtureError = fmt.Errorf("build native updater fixture: %w\n%s", err, output)
			return
		}
		coreProbeFixtureBinary, coreProbeFixtureError = os.ReadFile(path)
	})
	if coreProbeFixtureError != nil {
		t.Fatal(coreProbeFixtureError)
	}
	return coreProbeFixtureBinary
}

func TestStagedCoreVersionProbeRunsNativeExecutable(t *testing.T) {
	const version = "mihomo-optimized-v1.0.1"
	payload := coreTestExecutable(t)
	for _, test := range []struct {
		behavior, wantError string
	}{
		{"", ""},
		{"empty", "reported"},
		{"wrong-version", "expected " + version},
		{"wrong-platform", "unsupported-architecture"},
		{"legacy-prefix", ""},
		{"legacy-prefix-wrong-version", "expected " + version},
		{"legacy-prefix-wrong-platform", "unsupported-architecture"},
		{"unexpected-prefix", "reported"},
		{"split-version-line", "reported"},
		{"failure", "fixture CPU initialization failed"},
		{"excess-output", "output exceeds"},
		{"hang", "timed out"},
		{"invalid-executable", "cannot run -v"},
		{"no-execute-permission", "cannot run -v"},
	} {
		t.Run(test.behavior, func(t *testing.T) {
			if test.behavior == "no-execute-permission" && runtime.GOOS == "windows" {
				t.Skip("Windows does not use Unix execute permission bits")
			}
			t.Setenv("MIHOMO_TEST_CORE_VERSION", version)
			t.Setenv("MIHOMO_TEST_CORE_PROBE", test.behavior)
			path := filepath.Join(t.TempDir(), "staged-core.exe")
			data, mode := payload, os.FileMode(0o751)
			if test.behavior == "invalid-executable" {
				data = []byte("this release checksum can match without containing an executable")
			}
			if test.behavior == "no-execute-permission" {
				mode = 0o640
			}
			if err := os.WriteFile(path, data, mode); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if test.behavior == "hang" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			started := time.Now()
			err := validateStagedCoreContext(ctx, path, version)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("native version-only probe failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("invalid staged core escaped preflight: %v", err)
			}
			if test.behavior == "hang" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second) {
				t.Fatalf("version-only probe did not honor its deadline: %v", err)
			}
		})
	}
}
