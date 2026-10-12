// This native executable is installed only inside updater test directories.
// Its version-only interface verifies that preflight never passes the daemon's
// configuration arguments or starts another copy of the service.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "-v" {
		fmt.Fprintln(os.Stderr, "fixture accepts only -v")
		os.Exit(90)
	}
	version := os.Getenv("MIHOMO_TEST_CORE_VERSION")
	goos, goarch := runtime.GOOS, runtime.GOARCH
	prefix := ""
	switch os.Getenv("MIHOMO_TEST_CORE_PROBE") {
	case "empty":
		return
	case "wrong-version":
		version = "mihomo-optimized-v99.0.0"
	case "wrong-platform":
		goarch = "unsupported-architecture"
	case "legacy-prefix":
		prefix = "Mihomo Meta "
	case "legacy-prefix-wrong-version":
		prefix, version = "Mihomo Meta ", "mihomo-optimized-v99.0.0"
	case "legacy-prefix-wrong-platform":
		prefix, goarch = "Mihomo Meta ", "unsupported-architecture"
	case "unexpected-prefix":
		prefix = "Unknown core "
	case "split-version-line":
		fmt.Printf("%s\n%s %s with native updater fixture\n", version, goos, goarch)
		return
	case "failure":
		fmt.Fprintln(os.Stderr, "fixture CPU initialization failed")
		os.Exit(2)
	case "hang":
		time.Sleep(time.Minute)
	case "excess-output":
		fmt.Print(strings.Repeat("x", 32*1024))
		fmt.Fprint(os.Stderr, strings.Repeat("y", 32*1024))
	}
	fmt.Printf("%s%s %s %s with native updater fixture\n", prefix, version, goos, goarch)
}
