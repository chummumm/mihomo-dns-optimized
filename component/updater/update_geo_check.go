package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/geodata"
	_ "github.com/metacubex/mihomo/component/geodata/standard"
	"github.com/metacubex/mihomo/component/mmdb"
	"github.com/metacubex/mihomo/component/resource"
	C "github.com/metacubex/mihomo/constant"

	"github.com/oschwald/maxminddb-golang"
	"golang.org/x/sync/errgroup"
)

const maxGeoDataSize int64 = 512 << 20

// GeoUpdateConfig is captured from a successfully applied configuration. It
// deliberately contains no caller-supplied URL or path from the controller API.
// Commit protects only the final write/cache replacement, not network IO.
type GeoUpdateConfig struct {
	Usage       geodata.Usage
	UsageNow    func() geodata.Usage
	GeodataMode bool
	GeoIPURL    string
	MMDBURL     string
	GeoSiteURL  string
	ASNURL      string
	Context     context.Context
	Commit      func(func() error) error
}

type GeoUpdateResult struct {
	Name    string
	Changed bool
	Err     error
}

type geoDownloadError struct{ cause error }

func (e geoDownloadError) Error() string { return "GeoData download failed" }
func (e geoDownloadError) Unwrap() error { return e.cause }

type configuredGeoUpdates struct {
	config GeoUpdateConfig
	ctx    context.Context
	cancel context.CancelFunc
}

var configuredGeo atomic.Pointer[configuredGeoUpdates]

// ConfigureGeoUpdates also cancels automatic downloads from the previous
// configuration. A snapshot's Commit remains authoritative at the write point.
func ConfigureGeoUpdates(config GeoUpdateConfig) {
	parent := config.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	previous := configuredGeo.Swap(&configuredGeoUpdates{config: config, ctx: ctx, cancel: cancel})
	if previous != nil {
		previous.cancel()
	}
}

func currentGeoUpdates() (GeoUpdateConfig, context.Context) {
	if current := configuredGeo.Load(); current != nil {
		return current.config, current.ctx
	}
	// Embedded integrations which have not published a config snapshot retain
	// the existing getter-based behavior.
	return GeoUpdateConfig{
		Usage: geodata.CurrentUsage(), GeodataMode: geodata.GeodataMode(),
		GeoIPURL: geodata.GeoIpUrl(), MMDBURL: geodata.MmdbUrl(),
		GeoSiteURL: geodata.GeoSiteUrl(), ASNURL: geodata.ASNUrl(),
	}, context.Background()
}

func GeoDatabasesForConfig(config GeoUpdateConfig) []string {
	usage := config.Usage
	if config.UsageNow != nil {
		usage = config.UsageNow()
	}
	names := make([]string, 0, 3)
	if usage.GeoIP {
		if config.GeodataMode {
			names = append(names, "geoip")
		} else {
			names = append(names, "mmdb")
		}
	}
	if usage.ASN {
		names = append(names, "asn")
	}
	if usage.GeoSite {
		names = append(names, "geosite")
	}
	return names
}

func ActiveGeoDatabases() []string {
	config, _ := currentGeoUpdates()
	return GeoDatabasesForConfig(config)
}

func CheckAndUpdateGeoDatabases(ctx context.Context) ([]GeoUpdateResult, error) {
	config, lifetime := currentGeoUpdates()
	if err := lifetime.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	defer stop()
	defer cancel()
	return checkAndUpdateGeoDatabases(ctx, config)
}

// CheckAndUpdateGeoDatabasesContext returns a result for every enabled family,
// including independent failures. Unused default download sources are omitted.
func CheckAndUpdateGeoDatabasesContext(ctx context.Context, config GeoUpdateConfig) ([]GeoUpdateResult, error) {
	if config.Context != nil {
		merged, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(config.Context, cancel)
		defer stop()
		defer cancel()
		ctx = merged
	}
	var results []GeoUpdateResult
	err := geoUpdateGate.runWhenAvailable(ctx, ErrGetDatabaseUpdateSkip, func() error {
		var err error
		results, err = checkAndUpdateGeoDatabases(ctx, config)
		return err
	})
	return results, err
}

func checkAndUpdateGeoDatabases(ctx context.Context, config GeoUpdateConfig) ([]GeoUpdateResult, error) {
	if !geoUpdateGate.TryLock() {
		return nil, ErrGetDatabaseUpdateSkip
	}
	defer geoUpdateGate.Unlock()
	names := GeoDatabasesForConfig(config)
	results := make([]GeoUpdateResult, len(names))
	var tasks errgroup.Group
	for i, name := range names {
		i, name := i, name
		tasks.Go(func() error {
			changed, err := checkAndUpdateGeoDatabase(ctx, config, name)
			results[i] = GeoUpdateResult{Name: name, Changed: changed, Err: err}
			return nil
		})
	}
	_ = tasks.Wait()
	var allErrors []error
	for _, result := range results {
		if result.Err != nil {
			allErrors = append(allErrors, fmt.Errorf("%s: %w", result.Name, result.Err))
		}
	}
	return results, errors.Join(allErrors...)
}

func updateOneGeoDatabase(name string) error {
	if !geoUpdateGate.TryLock() {
		return ErrGetDatabaseUpdateSkip
	}
	defer geoUpdateGate.Unlock()
	config, ctx := currentGeoUpdates()
	_, err := checkAndUpdateGeoDatabase(ctx, config, name)
	return err
}

func checkAndUpdateGeoDatabase(ctx context.Context, config GeoUpdateConfig, name string) (bool, error) {
	if config.Context != nil {
		if err := config.Context.Err(); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var source, path string
	switch name {
	case "geoip":
		source, path = config.GeoIPURL, C.Path.GeoIP()
	case "mmdb":
		source, path = config.MMDBURL, C.Path.MMDB()
	case "asn":
		source, path = config.ASNURL, C.Path.ASN()
	case "geosite":
		source, path = config.GeoSiteURL, C.Path.GeoSite()
	default:
		return false, errors.New("unknown GeoData family")
	}
	if source == "" {
		return false, errors.New("GeoData source is not configured")
	}
	vehicle := resource.NewHTTPVehicle(source, path, "", nil, defaultHttpTimeout, maxGeoDataSize)
	oldData, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("cannot read current GeoData: %w", err)
	}
	var oldHash utils.HashType
	if err == nil {
		oldHash = utils.MakeHash(oldData)
	}
	data, hash, err := vehicle.Read(ctx, oldHash)
	if err != nil {
		// HTTP errors can contain credentials from a configured URL. Keep their
		// cause for cancellation classification without logging that URL.
		return false, geoDownloadError{cause: err}
	}
	commit := func(apply func() error) error {
		checked := func() error {
			if config.Context != nil {
				if err := config.Context.Err(); err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return apply()
		}
		if config.Commit != nil {
			return config.Commit(checked)
		}
		return checked()
	}
	if oldHash.Equal(hash) {
		return false, commit(func() error { return nil })
	}
	if len(data) == 0 {
		return false, errors.New("empty GeoData response")
	}
	if err := validateGeoData(name, data); err != nil {
		return false, err
	}
	err = commit(func() error {
		write := func() error { return vehicle.Write(data) }
		switch name {
		case "mmdb":
			return mmdb.ReplaceIP(path, data, oldData, write)
		case "asn":
			return mmdb.ReplaceASN(path, data, oldData, write)
		default:
			if err := write(); err != nil {
				return err
			}
			if name == "geoip" {
				geodata.ClearGeoIPCache()
			} else {
				geodata.ClearGeoSiteCache()
			}
			return nil
		}
	})
	return err == nil, err
}

func validateGeoData(name string, data []byte) error {
	if name == "mmdb" || name == "asn" {
		instance, err := maxminddb.FromBytes(data)
		if err != nil {
			return fmt.Errorf("invalid GeoData database: %w", err)
		}
		defer instance.Close()
		return nil
	}
	loader, err := geodata.GetGeoDataLoader("standard")
	if err != nil {
		return err
	}
	if name == "geoip" {
		_, err = loader.LoadIPByBytes(data, "cn")
	} else {
		_, err = loader.LoadSiteByBytes(data, "cn")
	}
	if err != nil {
		return fmt.Errorf("invalid GeoData database: %w", err)
	}
	return nil
}
