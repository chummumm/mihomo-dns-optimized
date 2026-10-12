package route

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
	return executor.Restart(func() {
		render.JSON(w, r, render.M{"status": "ok"})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		restartProcess(execPath)
	})
}

func restartProcess(execPath string) {
	var err error
	if runtime.GOOS == "windows" {
		cmd := exec.Command(execPath, os.Args[1:]...)
		log.Infoln("restarting: %q %q", execPath, os.Args[1:])
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Start()
		if err != nil {
			log.Fatalln("restarting: %s", err)
		}

		os.Exit(0)
	}

	log.Infoln("restarting: %q %q", execPath, os.Args[1:])
	err = syscall.Exec(execPath, os.Args, os.Environ())
	if err != nil {
		log.Fatalln("restarting: %s", err)
	}
}
