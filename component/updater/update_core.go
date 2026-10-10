package updater

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/ca"
	mihomoHttp "github.com/metacubex/mihomo/component/http"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/http"
)

const (
	// Bound both the downloaded archive and its decompressed executable.
	MaxPackageFileSize = 64 * 1024 * 1024
	maxCoreFileSize    = 256 * 1024 * 1024
	maxReleaseMetadata = 1024 * 1024

	ReleaseChannel = "release"
	AlphaChannel   = "alpha"
)

// CoreUpdater installs a checksummed release from this fork. It never falls
// back to the official core. The empty value uses the production release URL.
type CoreUpdater struct {
	mu sync.Mutex

	// Tests can serve a complete release locally without changing global HTTP
	// routing or executing the installed fixture. This is not a config option.
	releaseURL string
}

var DefaultCoreUpdater = CoreUpdater{}

func (u *CoreUpdater) Update(currentExePath string, channel string, force bool) (err error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	switch strings.ToLower(channel) {
	case "", "auto", ReleaseChannel:
	case AlphaChannel:
		return fmt.Errorf("DNS optimized has no alpha update channel; use release")
	default:
		return fmt.Errorf("unsupported DNS optimized update channel %q", channel)
	}
	currentExePath, err = filepath.EvalSymlinks(currentExePath)
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	info, err := os.Stat(currentExePath)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("not a regular file")
		}
		return fmt.Errorf("check current executable: %w", err)
	}
	target, err := coreTarget(runtime.GOOS, runtime.GOARCH, coreBuildSettings())
	if err != nil {
		return err
	}
	baseURL := u.releaseURL
	if baseURL == "" {
		baseURL = coreReleaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/") + "/"
	latestVersion, err := u.getLatestVersion(baseURL + "latest/download/version.txt")
	if err != nil {
		return fmt.Errorf("get latest DNS optimized version: %w", err)
	}
	latest, err := parseReleaseVersion(latestVersion)
	if err != nil {
		return err
	}
	log.Infoln("current version %s, latest DNS optimized version %s", C.Version, latestVersion)
	if !force {
		if latestVersion == C.Version {
			// Some dashboards depend on this existing error text.
			return fmt.Errorf("update error: already using latest version %s", C.Version)
		}
		if current, parseErr := parseReleaseVersion(C.Version); parseErr == nil && latest.compare(current) < 0 {
			return fmt.Errorf("refusing DNS optimized downgrade from %s to %s without force", C.Version, latestVersion)
		}
	}

	// Only the initial version lookup uses Latest. All subsequent requests are
	// pinned to the validated immutable tag, even if Latest changes meanwhile.
	releaseURL := baseURL + "download/" + latestVersion + "/"
	sums, err := u.readReleaseFile(releaseURL+"SHA256SUMS", maxReleaseMetadata)
	if err != nil {
		return fmt.Errorf("get release checksums: %w", err)
	}
	asset, err := selectCoreReleaseAsset(sums, target, latestVersion, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	packageName, exeName := asset.name, asset.executable

	// Stage on the same filesystem so replacement can use a rename. No
	// existing executable or backup is touched before verification succeeds.
	workDir := filepath.Dir(currentExePath)
	updateDir, err := os.MkdirTemp(workDir, ".mihomo-update-")
	if err != nil {
		return fmt.Errorf("create update directory: %w", err)
	}
	defer os.RemoveAll(updateDir)
	packagePath := filepath.Join(updateDir, packageName)
	if err = u.download(packagePath, releaseURL+packageName, asset.checksum); err != nil {
		return fmt.Errorf("download DNS optimized core: %w", err)
	}
	updateExePath := filepath.Join(updateDir, exeName)
	if err = u.unpack(packagePath, updateExePath, exeName, info.Mode().Perm()); err != nil {
		return fmt.Errorf("unpack DNS optimized core: %w", err)
	}
	if runtime.GOOS == "darwin" {
		if err = exec.Command("/usr/bin/codesign", "--force", "--sign", "-", updateExePath).Run(); err != nil {
			return fmt.Errorf("sign staged executable: %w", err)
		}
	}
	if err = u.install(currentExePath, updateExePath); err != nil {
		return fmt.Errorf("install DNS optimized core: %w", err)
	}
	log.Infoln("updater: installed DNS optimized %s", latestVersion)
	return nil
}

func (u *CoreUpdater) readReleaseFile(url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := mihomoHttp.HttpRequest(ctx, url, http.MethodGet, nil, nil,
		mihomoHttp.WithCAOption(ca.Option{ZeroTrust: true}))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release request returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("release metadata exceeds %d bytes", limit)
	}
	return body, nil
}

func (u *CoreUpdater) getLatestVersion(url string) (string, error) {
	body, err := u.readReleaseFile(url, 256)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(body))
	if _, err = parseReleaseVersion(version); err != nil {
		return "", err
	}
	return version, nil
}

func (u *CoreUpdater) download(path, url string, expected []byte) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := mihomoHttp.HttpRequest(ctx, url, http.MethodGet, nil, nil,
		mihomoHttp.WithCAOption(ca.Option{ZeroTrust: true}))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("archive request returned HTTP %d", resp.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, MaxPackageFileSize+1))
	if err != nil {
		return err
	}
	if n > MaxPackageFileSize {
		return fmt.Errorf("archive exceeds %d bytes", MaxPackageFileSize)
	}
	if !bytes.Equal(hash.Sum(nil), expected) {
		return fmt.Errorf("SHA256 mismatch for %s", filepath.Base(path))
	}
	return file.Sync()
}

// unpack writes exactly one executable to a caller-selected path. Gzip header
// names are ignored; Windows archives must contain the precise release member.
func (u *CoreUpdater) unpack(packagePath, outputPath, exeName string, mode os.FileMode) error {
	switch {
	case strings.HasSuffix(packagePath, ".gz"):
		file, err := os.Open(packagePath)
		if err != nil {
			return err
		}
		defer file.Close()
		reader, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer reader.Close()
		return writeStagedCore(outputPath, reader, mode)
	case strings.HasSuffix(packagePath, ".zip"):
		archive, err := zip.OpenReader(packagePath)
		if err != nil {
			return err
		}
		defer archive.Close()
		if len(archive.File) != 1 {
			return fmt.Errorf("expected exactly one executable in the archive")
		}
		member := archive.File[0]
		if member.Name != exeName || !member.Mode().IsRegular() || member.UncompressedSize64 > maxCoreFileSize {
			return fmt.Errorf("unexpected executable in the archive: %q", member.Name)
		}
		reader, err := member.Open()
		if err != nil {
			return err
		}
		defer reader.Close()
		return writeStagedCore(outputPath, reader, mode)
	default:
		return fmt.Errorf("unsupported archive format")
	}
}

func writeStagedCore(path string, reader io.Reader, mode os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	n, err := io.Copy(file, io.LimitReader(reader, maxCoreFileSize+1))
	if err != nil {
		return err
	}
	if n == 0 || n > maxCoreFileSize {
		return fmt.Errorf("invalid executable size: %d", n)
	}
	if err = file.Chmod(mode); err != nil {
		return err
	}
	return file.Sync()
}

func (u *CoreUpdater) install(currentPath, stagedPath string) error {
	backupDir := filepath.Join(filepath.Dir(currentPath), "meta-backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return err
	}
	backupPath := filepath.Join(backupDir, filepath.Base(currentPath))
	if runtime.GOOS == "windows" {
		// A running Windows executable must first be moved out of the way.
		if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove previous backup: %w", err)
		}
		if err := os.Rename(currentPath, backupPath); err != nil {
			return fmt.Errorf("back up running executable: %w", err)
		}
		if err := os.Rename(stagedPath, currentPath); err != nil {
			if restoreErr := os.Rename(backupPath, currentPath); restoreErr != nil {
				return fmt.Errorf("replace: %v; restore: %v; backup remains at %s", err, restoreErr, backupPath)
			}
			return err
		}
		return nil
	}
	// Unix permits replacing a running executable by rename; no live binary
	// is truncated, and failed staging or rename leaves it usable.
	source, err := os.Open(currentPath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	backup, err := os.CreateTemp(backupDir, ".mihomo-backup-")
	if err != nil {
		return err
	}
	backupTemp := backup.Name()
	defer os.Remove(backupTemp)
	_, copyErr := io.Copy(backup, source)
	if copyErr == nil {
		copyErr = backup.Chmod(info.Mode().Perm())
	}
	if copyErr == nil {
		copyErr = backup.Sync()
	}
	closeErr := backup.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(backupTemp, backupPath); err != nil {
		return err
	}
	return os.Rename(stagedPath, currentPath)
}
