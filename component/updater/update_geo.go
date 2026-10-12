package updater

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

var (
	autoUpdate     atomic.Bool
	updateInterval atomic.Int64

	geoUpdateGate updateOperationGate
)

func GeoAutoUpdate() bool {
	return autoUpdate.Load()
}

func GeoUpdateInterval() int {
	return int(updateInterval.Load())
}

func SetGeoAutoUpdate(newAutoUpdate bool) {
	autoUpdate.Store(newAutoUpdate)
}

func SetGeoUpdateInterval(newGeoUpdateInterval int) {
	updateInterval.Store(int64(newGeoUpdateInterval))
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

func getUpdateTime() (time time.Time, err error) {
	filesToCheck := []string{
		C.Path.GeoIP(),
		C.Path.MMDB(),
		C.Path.ASN(),
		C.Path.GeoSite(),
	}

	for _, file := range filesToCheck {
		var fileInfo os.FileInfo
		fileInfo, err = os.Stat(file)
		if err == nil {
			return fileInfo.ModTime(), nil
		}
	}

	return
}

func RegisterGeoUpdater() {
	if GeoUpdateInterval() <= 0 {
		log.Errorln("[GEO] Invalid update interval: %d", GeoUpdateInterval())
		return
	}

	go func() {
		ticker := time.NewTicker(time.Duration(GeoUpdateInterval()) * time.Hour)
		defer ticker.Stop()

		lastUpdate, err := getUpdateTime()
		if err != nil {
			log.Errorln("[GEO] Get GEO database update time error: %s", err.Error())
			return
		}

		log.Infoln("[GEO] last update time %s", lastUpdate)
		if lastUpdate.Add(time.Duration(GeoUpdateInterval()) * time.Hour).Before(time.Now()) {
			log.Infoln("[GEO] Database has not been updated for %v, update now", time.Duration(GeoUpdateInterval())*time.Hour)
			if err := UpdateGeoDatabases(); err != nil {
				log.Errorln("[GEO] Failed to update GEO database: %s", err.Error())
				return
			}
		}

		for range ticker.C {
			log.Infoln("[GEO] updating database every %d hours", GeoUpdateInterval())
			if err := UpdateGeoDatabases(); err != nil {
				log.Errorln("[GEO] Failed to update GEO database: %s", err.Error())
			}
		}
	}()
}
