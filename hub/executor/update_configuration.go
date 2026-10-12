package executor

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/metacubex/mihomo/component/geodata"
	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
	P "github.com/metacubex/mihomo/constant/provider"
)

var ErrUpdateConfigurationChanged = errors.New("update configuration changed")

// UpdateConfiguration is published after ApplyConfig resumes the applied
// routing, DNS and updater configuration (listener errors are reported apart). Its
// immutable resource identities never observe a candidate parser's temporary
// GeoData URLs or a half-applied provider map. Networking uses Context; only
// final file/strategy transactions use Commit.
type UpdateConfiguration struct {
	Context       context.Context
	RuleProviders map[string]P.RuleProvider
	Geo           updater.GeoUpdateConfig
	UI            *updater.UIUpdater
	General       config.General
	cancel        context.CancelFunc
}

var appliedUpdateConfiguration atomic.Pointer[UpdateConfiguration]
var updateConfigurationCommitMu sync.Mutex

func CurrentUpdateConfiguration() *UpdateConfiguration {
	return appliedUpdateConfiguration.Load()
}

func (s *UpdateConfiguration) Commit(action func() error) error {
	updateConfigurationCommitMu.Lock()
	defer updateConfigurationCommitMu.Unlock()
	if s.Context.Err() != nil || appliedUpdateConfiguration.Load() != s {
		return ErrUpdateConfigurationChanged
	}
	return action()
}

// Called at ApplyConfig/shutdown entry. The lock is never held during network
// reads or provider initialization, so invalidated downloads unwind promptly.
func invalidateUpdateConfiguration() {
	updateConfigurationCommitMu.Lock()
	defer updateConfigurationCommitMu.Unlock()
	if previous := appliedUpdateConfiguration.Load(); previous != nil {
		previous.cancel()
	}
}

func publishUpdateConfiguration(cfg *config.Config) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &UpdateConfiguration{
		Context: ctx, cancel: cancel,
		RuleProviders: maps.Clone(cfg.RuleProviders),
		UI:            updater.CurrentUIUpdater(),
		General:       *cfg.General,
		Geo: updater.GeoUpdateConfig{
			Context: ctx,
			Usage:   config.CalculateGeoDataUsage(cfg), GeodataMode: cfg.General.GeodataMode,
			GeoIPURL: cfg.General.GeoXUrl.GeoIp, MMDBURL: cfg.General.GeoXUrl.Mmdb,
			GeoSiteURL: cfg.General.GeoXUrl.GeoSite, ASNURL: cfg.General.GeoXUrl.ASN,
		},
	}
	s.Geo.UsageNow = func() geodata.Usage {
		usage := s.Geo.Usage
		for _, provider := range s.RuleProviders {
			usage = usage.Merge(geodata.UsageForProvider(provider))
		}
		return usage
	}
	s.Geo.Commit = s.Commit
	for _, provider := range s.RuleProviders {
		if binding, ok := provider.(interface {
			BindUpdateContext(context.Context, func(func() error) error)
		}); ok {
			binding.BindUpdateContext(ctx, s.Commit)
		}
	}
	updateConfigurationCommitMu.Lock()
	appliedUpdateConfiguration.Store(s)
	updater.ConfigureGeoUpdates(s.Geo)
	updateConfigurationCommitMu.Unlock()
}
