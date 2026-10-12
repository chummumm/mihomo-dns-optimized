package geodata

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/mmdb"
	"github.com/metacubex/mihomo/component/resource"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"github.com/oschwald/maxminddb-golang"
)

var (
	initGeoSite bool
	initGeoIP   int
	initASN     bool

	initGeoSiteMutex sync.Mutex
	initGeoIPMutex   sync.Mutex
	initASNMutex     sync.Mutex

	geoIpEnable   atomic.Bool
	geoSiteEnable atomic.Bool
	asnEnable     atomic.Bool

	geoIpUrl   string
	mmdbUrl    string
	geoSiteUrl string
	asnUrl     string
)

func GeoIpUrl() string {
	return geoIpUrl
}

func SetGeoIpUrl(url string) {
	geoIpUrl = url
}

func MmdbUrl() string {
	return mmdbUrl
}

func SetMmdbUrl(url string) {
	mmdbUrl = url
}

func GeoSiteUrl() string {
	return geoSiteUrl
}

func SetGeoSiteUrl(url string) {
	geoSiteUrl = url
}

func ASNUrl() string {
	return asnUrl
}

func SetASNUrl(url string) {
	asnUrl = url
}

func downloadToPath(url string, path string) (err error) {
	defer func() { recordInitializationError(err) }()
	scope := currentInitializationContext()
	ctx, cancel := context.WithTimeout(scope.ctx, time.Second*90)
	defer cancel()
	vehicle := resource.NewHTTPVehicle(url, path, "", nil, 90*time.Second, 512<<20)
	data, _, err := vehicle.Read(ctx, utils.HashType{})
	if err != nil {
		return initialDownloadError{cause: err}
	}
	if err := validateInitialGeoData(path, data); err != nil {
		return err
	}
	apply := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		write := func() error { return vehicle.Write(data) }
		switch path {
		case C.Path.MMDB(), C.Path.ASN():
			old, err := os.ReadFile(path)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if path == C.Path.MMDB() {
				return mmdb.ReplaceIP(path, data, old, write)
			}
			return mmdb.ReplaceASN(path, data, old, write)
		default:
			if err := write(); err != nil {
				return err
			}
			if path == C.Path.GeoIP() {
				ClearGeoIPCache()
			} else {
				ClearGeoSiteCache()
			}
			return nil
		}
	}
	if scope.commit != nil {
		return scope.commit(apply)
	}
	return apply()
}

type initialDownloadError struct{ cause error }

func (e initialDownloadError) Error() string { return "GeoData download failed" }
func (e initialDownloadError) Unwrap() error { return e.cause }

func validateInitialGeoData(path string, data []byte) error {
	if path == C.Path.MMDB() || path == C.Path.ASN() {
		reader, err := maxminddb.FromBytes(data)
		if err != nil {
			return fmt.Errorf("invalid GeoData database: %w", err)
		}
		return reader.Close()
	}
	loader, err := GetGeoDataLoader("standard")
	if err != nil {
		return err
	}
	if path == C.Path.GeoIP() {
		_, err = loader.LoadIPByBytes(data, "cn")
	} else {
		_, err = loader.LoadSiteByBytes(data, "cn")
	}
	return err
}

func InitGeoSite() (err error) {
	defer func() { recordInitializationError(err) }()
	geoSiteEnable.Store(true)
	initGeoSiteMutex.Lock()
	defer initGeoSiteMutex.Unlock()
	if _, err := os.Stat(C.Path.GeoSite()); os.IsNotExist(err) {
		log.Infoln("Can't find GeoSite.dat, start download")
		if err := downloadToPath(GeoSiteUrl(), C.Path.GeoSite()); err != nil {
			return fmt.Errorf("can't download GeoSite.dat: %s", err.Error())
		}
		log.Infoln("Download GeoSite.dat finish")
		initGeoSite = false
	}
	if !initGeoSite {
		if err := Verify(C.GeositeName); err != nil {
			log.Warnln("GeoSite.dat invalid, download replacement: %s", err)
			if err := downloadToPath(GeoSiteUrl(), C.Path.GeoSite()); err != nil {
				return fmt.Errorf("can't download GeoSite.dat: %s", err.Error())
			}
		}
		initGeoSite = true
	}
	return nil
}

func InitGeoIP() (err error) {
	defer func() { recordInitializationError(err) }()
	geoIpEnable.Store(true)
	initGeoIPMutex.Lock()
	defer initGeoIPMutex.Unlock()
	if GeodataMode() {
		if _, err := os.Stat(C.Path.GeoIP()); os.IsNotExist(err) {
			log.Infoln("Can't find GeoIP.dat, start download")
			if err := downloadToPath(GeoIpUrl(), C.Path.GeoIP()); err != nil {
				return fmt.Errorf("can't download GeoIP.dat: %s", err.Error())
			}
			log.Infoln("Download GeoIP.dat finish")
			initGeoIP = 0
		}

		if initGeoIP != 1 {
			if err := Verify(C.GeoipName); err != nil {
				log.Warnln("GeoIP.dat invalid, download replacement: %s", err)
				if err := downloadToPath(GeoIpUrl(), C.Path.GeoIP()); err != nil {
					return fmt.Errorf("can't download GeoIP.dat: %s", err.Error())
				}
			}
			initGeoIP = 1
		}
		return nil
	}

	if _, err := os.Stat(C.Path.MMDB()); os.IsNotExist(err) {
		log.Infoln("Can't find MMDB, start download")
		if err := downloadToPath(MmdbUrl(), C.Path.MMDB()); err != nil {
			return fmt.Errorf("can't download MMDB: %s", err.Error())
		}
	}

	if initGeoIP != 2 {
		if !mmdb.Verify(C.Path.MMDB()) {
			log.Warnln("MMDB invalid, download replacement")
			if err := downloadToPath(MmdbUrl(), C.Path.MMDB()); err != nil {
				return fmt.Errorf("can't download MMDB: %s", err.Error())
			}
		}
		initGeoIP = 2
	}
	return nil
}

func InitASN() (err error) {
	defer func() { recordInitializationError(err) }()
	asnEnable.Store(true)
	initASNMutex.Lock()
	defer initASNMutex.Unlock()
	if _, err := os.Stat(C.Path.ASN()); os.IsNotExist(err) {
		log.Infoln("Can't find ASN.mmdb, start download")
		if err := downloadToPath(ASNUrl(), C.Path.ASN()); err != nil {
			return fmt.Errorf("can't download ASN.mmdb: %s", err.Error())
		}
		log.Infoln("Download ASN.mmdb finish")
		initASN = false
	}
	if !initASN {
		if !mmdb.Verify(C.Path.ASN()) {
			log.Warnln("ASN invalid, download replacement")
			if err := downloadToPath(ASNUrl(), C.Path.ASN()); err != nil {
				return fmt.Errorf("can't download ASN: %s", err.Error())
			}
		}
		initASN = true
	}
	return nil
}

func GeoIpEnable() bool {
	return geoIpEnable.Load()
}

func GeoSiteEnable() bool {
	return geoSiteEnable.Load()
}

func ASNEnable() bool {
	return asnEnable.Load()
}
