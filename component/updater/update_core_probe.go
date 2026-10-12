package updater

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	maxCoreProbeOutput = 4 * 1024
	coreProbeTimeout   = 10 * time.Second
)

// validateStagedCoreContext runs only the version command, which exits before
// configuration loading or listener setup. A wrong CPU ABI, a noexec mount or
// an unusable release must fail while the original core is still serving.
func validateStagedCoreContext(parent context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(parent, coreProbeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, "-v")
	// Also bound waiting on inherited output pipes if a broken executable
	// starts a child and exits without closing them.
	command.WaitDelay = time.Second
	output := &coreProbeOutput{}
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("staged core -v was canceled or timed out: %w", ctx.Err())
		}
		return fmt.Errorf("staged core cannot run -v: %w; output: %q", err, output.text.String())
	}
	if output.exceeded {
		return fmt.Errorf("staged core -v output exceeds %d bytes", maxCoreProbeOutput)
	}
	// Historical fork releases retain upstream's fixed "Mihomo Meta" prefix.
	// Keep that compatibility while requiring the exact version and platform
	// together on the version line, rather than matching words across lines.
	versionLine, _, _ := strings.Cut(strings.TrimSpace(output.text.String()), "\n")
	fields := strings.Fields(versionLine)
	if len(fields) >= 2 && fields[0] == "Mihomo" && fields[1] == "Meta" {
		fields = fields[2:]
	}
	if len(fields) < 3 || fields[0] != version || fields[1] != runtime.GOOS || fields[2] != runtime.GOARCH {
		return fmt.Errorf("staged core -v reported %q; expected %s %s %s", output.text.String(), version, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// os/exec serializes writes when Stdout and Stderr share the same writer.
// Continue draining excess output so neither memory nor pipe backpressure can
// turn a broken version command into an unbounded update operation.
type coreProbeOutput struct {
	text     strings.Builder
	exceeded bool
}

func (w *coreProbeOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxCoreProbeOutput - w.text.Len()
	if len(p) > remaining {
		p = p[:remaining]
		w.exceeded = true
	}
	_, _ = w.text.Write(p)
	return n, nil
}
