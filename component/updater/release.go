package updater

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const coreReleaseURL = "https://github.com/chummumm/mihomo-dns-optimized/releases/"

var dnsReleaseVersion = regexp.MustCompile("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)-dns-optimized-([1-9][0-9]*)$")

type releaseVersion [4]uint64

func parseReleaseVersion(value string) (releaseVersion, error) {
	var version releaseVersion
	match := dnsReleaseVersion.FindStringSubmatch(value)
	if match == nil {
		return version, fmt.Errorf("unsupported DNS optimized release version %q", value)
	}
	for i := range version {
		number, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return version, fmt.Errorf("invalid DNS optimized release version: %w", err)
		}
		version[i] = number
	}
	return version, nil
}

func (v releaseVersion) compare(other releaseVersion) int {
	for i, number := range v {
		if number < other[i] {
			return -1
		}
		if number > other[i] {
			return 1
		}
	}
	return 0
}

func coreBuildSettings() map[string]string {
	settings := make(map[string]string)
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
	}
	return settings
}

// coreTarget preserves the CPU ABI of the running build. The matrix test checks
// every published target, including the v1 basename and soft-float variants.
func coreTarget(goos, goarch string, settings map[string]string) (string, error) {
	target := goos + "-" + goarch
	switch goarch {
	case "amd64":
		switch level := settings["GOAMD64"]; level {
		case "", "v1":
		case "v2", "v3":
			target += "-" + level
		default:
			return "", fmt.Errorf("no DNS optimized update for GOAMD64=%s", level)
		}
	case "386":
		switch mode := settings["GO386"]; mode {
		case "", "sse2":
		case "softfloat":
			target += "-softfloat"
		default:
			return "", fmt.Errorf("no DNS optimized update for GO386=%s", mode)
		}
	case "arm":
		level := settings["GOARM"]
		// Go records the implicit floating-point ABI along with the level.
		parts := strings.Split(level, ",")
		level = parts[0]
		if len(parts) > 2 || (len(parts) == 2 &&
			parts[1] != map[string]string{"5": "softfloat", "6": "hardfloat", "7": "hardfloat"}[level]) {
			return "", fmt.Errorf("unsupported ARM floating-point ABI %q", settings["GOARM"])
		}
		target = goos + "-armv" + level
	case "arm64":
		if goos == "android" {
			target += "-v8"
		}
	case "mips", "mipsle":
		mode := settings["GOMIPS"]
		if mode == "" {
			mode = "hardfloat"
		}
		target += "-" + mode
	case "mips64", "mips64le":
		if mode := settings["GOMIPS64"]; mode != "" && mode != "hardfloat" {
			return "", fmt.Errorf("no DNS optimized update for GOMIPS64=%s", mode)
		}
	case "loong64":
		target += "-abi2"
	}
	switch target {
	case "linux-386", "linux-386-softfloat",
		"linux-amd64", "linux-amd64-v2", "linux-amd64-v3", "linux-arm64",
		"linux-armv5", "linux-armv6", "linux-armv7",
		"linux-mips-hardfloat", "linux-mips-softfloat",
		"linux-mipsle-hardfloat", "linux-mipsle-softfloat",
		"linux-mips64", "linux-mips64le", "linux-loong64-abi2",
		"linux-riscv64", "linux-s390x", "linux-ppc64le",
		"windows-386", "windows-amd64", "windows-amd64-v2", "windows-amd64-v3", "windows-arm64",
		"darwin-amd64", "darwin-amd64-v2", "darwin-amd64-v3", "darwin-arm64",
		"freebsd-386", "freebsd-amd64", "freebsd-amd64-v2", "freebsd-amd64-v3", "freebsd-arm64",
		"android-386", "android-amd64", "android-armv7", "android-arm64-v8":
		return target, nil
	default:
		return "", fmt.Errorf("no DNS optimized release target for %s", target)
	}
}

func (u *CoreUpdater) CoreBaseName() string {
	target, err := coreTarget(runtime.GOOS, runtime.GOARCH, coreBuildSettings())
	if err != nil {
		return ""
	}
	return "mihomo-dns-" + target
}

func releaseChecksum(contents []byte, name string) ([]byte, error) {
	var checksum []byte
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid SHA256SUMS record")
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != 32 {
			return nil, fmt.Errorf("invalid SHA256SUMS digest")
		}
		if fields[1] != name {
			continue
		}
		if checksum != nil {
			return nil, fmt.Errorf("duplicate SHA256SUMS entry for %s", name)
		}
		checksum = sum
	}
	if checksum == nil {
		return nil, fmt.Errorf("release does not contain a checksum for %s", name)
	}
	return checksum, nil
}
