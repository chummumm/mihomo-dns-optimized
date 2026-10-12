package route

import (
	"errors"
	"fmt"
	"os"

	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/chi"
	"github.com/metacubex/chi/render"
	"github.com/metacubex/http"
)

func upgradeRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/check", getUpgradeCheck)
	r.Post("/check", startUpgradeCheck)
	r.Get("/ui", getUIUpdateStatus)
	r.Post("/ui", updateUI)
	if !embedMode { // disallow upgrade core/geo in embed mode
		r.Post("/", upgradeCore)
		r.Post("/geo", updateGeoDatabases)
	}
	return r
}

func upgradeCore(w http.ResponseWriter, r *http.Request) {
	// modify from https://github.com/AdguardTeam/AdGuardHome/blob/595484e0b3fb4c457f9bb727a6b94faa78a66c5f/internal/home/controlupdate.go#L108
	log.Infoln("start update")
	execPath, err := os.Executable()
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, newError(fmt.Sprintf("getting path: %s", err)))
		return
	}

	query := r.URL.Query()
	channel := query.Get("channel")
	force := query.Get("force") == "true"

	err = updater.DefaultCoreUpdater.UpdateContext(r.Context(), execPath, channel, force)
	if err != nil {
		log.Warnln("%s", err)
		status := http.StatusInternalServerError
		if errors.Is(err, updater.ErrCoreUpdateBusy) {
			status = http.StatusConflict
		}
		render.Status(r, status)
		render.JSON(w, r, newError(fmt.Sprintf("%s", err)))
		return
	}

	render.JSON(w, r, render.M{"status": "ok"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go restartExecutable(execPath)
}

func getUIUpdateStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	render.JSON(w, r, updater.CurrentUIUpdater().Status(r.Context(), r.URL.Query().Get("check") == "true"))
}

func updateUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	err := updater.CurrentUIUpdater().DownloadUIContext(r.Context())
	if err != nil {
		log.Warnln("%s", err)
		status := http.StatusInternalServerError
		if errors.Is(err, updater.ErrUIUpdateBusy) {
			status = http.StatusConflict
		}
		render.Status(r, status)
		render.JSON(w, r, newError(fmt.Sprintf("%s", err)))
		return
	}

	render.JSON(w, r, render.M{"status": "ok"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
