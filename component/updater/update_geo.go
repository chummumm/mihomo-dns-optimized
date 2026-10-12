package updater

import (
	"context"
	"errors"

	"github.com/metacubex/mihomo/log"
)

var geoUpdateGate updateOperationGate

func GeoAutoUpdate() bool {
	enabled, _ := DefaultGeoUpdater.AutomaticPolicy()
	return enabled
}

func GeoUpdateInterval() int {
	_, interval := DefaultGeoUpdater.AutomaticPolicy()
	return interval
}

func SetGeoAutoUpdate(newAutoUpdate bool) {
	u := &DefaultGeoUpdater
	u.mu.Lock()
	defer u.mu.Unlock()
	u.configureLocked(newAutoUpdate, u.updateIntervalLocked())
}

func SetGeoUpdateInterval(newGeoUpdateInterval int) {
	u := &DefaultGeoUpdater
	u.mu.Lock()
	defer u.mu.Unlock()
	u.configureLocked(u.autoUpdate, newGeoUpdateInterval)
}

// Existing entry points use the same gate and validated application path as
// the unified controller task and the automatic updater.
func UpdateMMDB() error    { return updateOneGeoDatabase("mmdb") }
func UpdateASN() error     { return updateOneGeoDatabase("asn") }
func UpdateGeoIp() error   { return updateOneGeoDatabase("geoip") }
func UpdateGeoSite() error { return updateOneGeoDatabase("geosite") }

var ErrGetDatabaseUpdateSkip = errors.New("GEO database is updating, skip")

func UpdateGeoDatabases() error {
	log.Infoln("[GEO] Start updating GEO database")
	_, err := CheckAndUpdateGeoDatabases(context.Background())
	if err != nil {
		log.Errorln("[GEO] update GEO database error: %s", err.Error())
	}
	return err
}

// RegisterGeoUpdater remains an idempotent entry point for embedded callers.
// Normal startup and reload configure the scheduler as part of ApplyConfig.
func RegisterGeoUpdater() {
	u := &DefaultGeoUpdater
	u.mu.Lock()
	defer u.mu.Unlock()
	u.configureLocked(u.autoUpdate, u.updateIntervalLocked())
}
