package route

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"

	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
)

func init() {
	updater.DefaultCoreUpdater.SetAutomaticRestartHandler(automaticCoreRestart)
}

func automaticCoreRestart(execPath string, commit func() error) error {
	return executor.ApplyUpdateAndRestart(commit, func() { restartProcess(execPath) })
}

func restartRouter() http.Handler {
	r := chi.NewRouter()
	r.Post("/", restart)
	return r
}

func restart(w http.ResponseWriter, r *http.Request) {
	// modify from https://github.com/AdguardTeam/AdGuardHome/blob/595484e0b3fb4c457f9bb727a6b94faa78a66c5f/internal/home/controlupdate.go#L108
	execPath, err := os.Executable()
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, newError(fmt.Sprintf("getting path: %s", err)))
		return
	}

	// Preparation can fail while keeping the old TUN alive. Do not acknowledge
	// a restart before that barrier succeeds, or allow a reload into the gap
	// between stopping its backends and replacing this process.
	if err := restartAndRespond(w, r, execPath); err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, newError(fmt.Sprintf("restart preparation failed: %s", err)))
	}
}

func restartAndRespond(w http.ResponseWriter, r *http.Request, execPath string) error {
	acknowledged := false
	err := executor.Restart(func() {
		writeRestartAcknowledgment(w)
		acknowledged = true
		restartProcess(execPath)
	})
	// A stop can supersede restart after the acknowledgment was flushed.
	// The response is already complete; appending a second error would corrupt it.
	if acknowledged && errors.Is(err, executor.ErrShuttingDown) {
		return nil
	}
	return err
}

func writeRestartAcknowledgment(w http.ResponseWriter) {
	const body = "{\"status\":\"ok\"}\n"
	// Exec does not return through the HTTP handler. Without an explicit length,
	// Flush starts a chunked response whose final chunk would never be written.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func restartProcess(execPath string) {
	if executor.ShutdownRequested() {
		return
	}
	var err error
	if runtime.GOOS == "windows" {
		cmd := exec.Command(execPath, os.Args[1:]...)
		log.Infoln("restarting: %q %q", execPath, os.Args[1:])
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if executor.ShutdownRequested() {
			return
		}
		err = cmd.Start()
		if err != nil {
			log.Fatalln("restarting: %s", err)
		}

		os.Exit(0)
	}

	log.Infoln("restarting: %q %q", execPath, os.Args[1:])
	if executor.ShutdownRequested() {
		return
	}
	err = syscall.Exec(execPath, os.Args, os.Environ())
	if err != nil {
		log.Fatalln("restarting: %s", err)
	}
}
