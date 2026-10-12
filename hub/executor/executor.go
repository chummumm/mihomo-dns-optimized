package executor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/adapter/outboundgroup"
	"github.com/metacubex/mihomo/component/auth"
	"github.com/metacubex/mihomo/component/ca"
	"github.com/metacubex/mihomo/component/dialer"
	"github.com/metacubex/mihomo/component/dnsstats"
	"github.com/metacubex/mihomo/component/geodata"
	mihomoHttp "github.com/metacubex/mihomo/component/http"
	"github.com/metacubex/mihomo/component/iface"
	"github.com/metacubex/mihomo/component/keepalive"
	"github.com/metacubex/mihomo/component/profile"
	"github.com/metacubex/mihomo/component/profile/cachefile"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/component/resource"
	"github.com/metacubex/mihomo/component/sniffer"
	"github.com/metacubex/mihomo/component/transparentproxy"
	"github.com/metacubex/mihomo/component/trie"
	"github.com/metacubex/mihomo/component/updater"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
	"github.com/metacubex/mihomo/dns"
	"github.com/metacubex/mihomo/listener"
	authStore "github.com/metacubex/mihomo/listener/auth"
	LC "github.com/metacubex/mihomo/listener/config"
	"github.com/metacubex/mihomo/listener/inner"
	"github.com/metacubex/mihomo/listener/tproxy"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/ntp/ntp"
	"github.com/metacubex/mihomo/tunnel"
)

var mux sync.Mutex

// ErrShuttingDown rejects new configuration work after a normal stop request.
var ErrShuttingDown = errors.New("mihomo is shutting down")

// Publish stop intent before waiting for mux: another goroutine may own mux
// while preparing an exec that would discard the waiting shutdown goroutine.
// Controlled restart failures do not publish a normal stop request.
var stopping atomic.Bool

// RequestShutdown closes admission immediately, including during startup or
// restart preparation. Shutdown performs the serialized resource cleanup.
func RequestShutdown() { stopping.Store(true) }

// ShutdownRequested permits a final check immediately before process exec.
func ShutdownRequested() bool { return stopping.Load() }

// Serialized by mux. A later configuration's activation must also wait for
// earlier resets, since they can still touch the shared system resolver.
var updaterResetDone <-chan struct{}

// Keep ownership of every resolver constructed by updateDNS, even when the
// local DNS service is disabled and only its proxy/bootstrap handle is exposed.
// ApplyConfig serializes replacement with mux.
var dnsResolverOwner *dns.Resolvers

func readConfig(path string) ([]byte, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("configuration file %s is empty", path)
	}

	return data, err
}

// Parse config with default config path
func Parse() (*config.Config, error) {
	return ParseWithPath(C.Path.Config())
}

// ParseWithPath parse config with custom config path
func ParseWithPath(path string) (*config.Config, error) {
	buf, err := readConfig(path)
	if err != nil {
		return nil, err
	}

	return ParseWithBytes(buf)
}

// ParseWithBytes config with buffer
func ParseWithBytes(buf []byte) (*config.Config, error) {
	return config.Parse(buf)
}

// ApplyConfig dispatch configure to all parts without ExternalController
func ApplyConfig(cfg *config.Config, force bool) error {
	mux.Lock()
	defer mux.Unlock()
	if ShutdownRequested() {
		return ErrShuttingDown
	}
	wasManaged := transparentproxy.IsEnabled()
	if err := beginTransparentProxyApply(cfg.General.TransparentProxy, transparentProxyRuntime(cfg)); err != nil {
		return fmt.Errorf("apply transparent-proxy: %w", err)
	}
	startTransparentProxyRepairs()
	invalidateUpdateConfiguration()
	log.SetLevel(cfg.General.LogLevel)
	if cfg.General.TransparentProxy.Enable {
		log.Infoln("[Transparent Proxy] managed capture enabled; effective tun.auto-route, tun.auto-redirect and iptables.enable are false")
	}
	finishUpdates := updateUpdater(cfg)

	tunnel.OnSuspend()
	// Cancel old DNS background work before replacing rules, nodes or pools.
	// Old DNS listeners may briefly remain reachable, but closed resolvers
	// reject new work until updateDNS installs the replacement service.
	closeDNSResolvers()

	ca.ResetCertificate()
	for _, c := range cfg.TLS.CustomTrustCert {
		if err := ca.AddCertificate(c); err != nil {
			log.Warnln("%s\nadd error: %s", c, err.Error())
		}
	}

	updateExperimental(cfg.Experimental)
	updateUsers(cfg.Users)
	updateProxies(cfg.Proxies, cfg.Providers)
	updateRules(cfg.Rules, cfg.SubRules, cfg.RuleProviders)
	updateSniffer(cfg.Sniffer)
	updateHosts(cfg.Hosts)
	_ = geodata.WithInitializationContext(context.Background(), nil, func() error {
		updateGeneral(cfg.General, true)
		return nil
	})
	// Do not change this in temporaryUpdateGeneral while merely parsing a
	// candidate config: the DNS routing flag and resolver policy change together.
	tunnel.SetDNSRuleRouting(cfg.General.DNSRuleRouting)
	if cfg.General.DNSRuleRouting {
		log.Infoln("DNS rule routing enabled; nameserver-policy and domain fallback policies are inactive")
	}
	updateDNS(cfg.DNS, cfg.General.IPv6)
	updateNTP(cfg.NTP) // initialize NTP after DNS because an NTP server may be a hostname.
	applyErr := updateListeners(cfg.General, cfg.Listeners, force, wasManaged)
	applyErr = errors.Join(applyErr, updateTun(cfg.General)) // tun should not care "force"
	updateIPTables(cfg)
	updateTunnels(cfg.Tunnels)

	tunnel.OnInnerLoading()

	initInnerTcp()
	loadProvider(cfg.Providers)
	updateProfile(cfg)
	loadProvider(cfg.RuleProviders)
	runtime.GC()
	tunnel.OnRunning()
	applyErr = errors.Join(applyErr, finishTransparentProxyApply(transparentProxyRuntime(cfg)))
	publishUpdateConfiguration(cfg)
	finishUpdates()
	return applyErr
}

func initInnerTcp() {
	inner.New(tunnel.Tunnel)
}

func GetGeneral() *config.General {
	ports := listener.GetPorts()
	uiUpdater := updater.CurrentUIUpdater()
	coreAutoUpdate, coreUpdateInterval := updater.DefaultCoreUpdater.AutomaticPolicy()
	geoAutoUpdate, geoUpdateInterval := updater.DefaultGeoUpdater.AutomaticPolicy()
	var authenticator []string
	if auth := authStore.Default.Authenticator(); auth != nil {
		authenticator = auth.Users()
	}

	general := &config.General{
		Inbound: config.Inbound{
			Port:              ports.Port,
			SocksPort:         ports.SocksPort,
			RedirPort:         ports.RedirPort,
			TProxyPort:        ports.TProxyPort,
			MixedPort:         ports.MixedPort,
			Tun:               listener.GetTunConf(),
			TuicServer:        listener.GetTuicConf(),
			ShadowSocksConfig: ports.ShadowSocksConfig,
			VmessConfig:       ports.VmessConfig,
			Authentication:    authenticator,
			SkipAuthPrefixes:  inbound.SkipAuthPrefixes(),
			LanAllowedIPs:     inbound.AllowedIPs(),
			LanDisAllowedIPs:  inbound.DisAllowedIPs(),
			AllowLan:          listener.AllowLan(),
			BindAddress:       listener.BindAddress(),
			InboundTfo:        inbound.Tfo(),
			InboundMPTCP:      inbound.MPTCP(),
		},
		DNSRuleRouting:   tunnel.DNSRuleRoutingEnabled(),
		Mode:             tunnel.Mode(),
		UnifiedDelay:     adapter.UnifiedDelay.Load(),
		LogLevel:         log.Level(),
		IPv6:             !resolver.DisableIPv6,
		Interface:        dialer.DefaultInterface.Load(),
		RoutingMark:      int(dialer.DefaultRoutingMark.Load()),
		TransparentProxy: transparentproxy.CurrentConfig(),
		GeoXUrl: config.GeoXUrl{
			GeoIp:   geodata.GeoIpUrl(),
			Mmdb:    geodata.MmdbUrl(),
			ASN:     geodata.ASNUrl(),
			GeoSite: geodata.GeoSiteUrl(),
		},
		GeoAutoUpdate:            geoAutoUpdate,
		GeoUpdateInterval:        geoUpdateInterval,
		ExternalUIAutoUpdate:     uiUpdater.AutoUpdate(),
		ExternalUIUpdateInterval: uiUpdater.UpdateInterval(),
		CoreAutoUpdate:           coreAutoUpdate,
		CoreUpdateInterval:       coreUpdateInterval,
		GeodataMode:              geodata.GeodataMode(),
		GeodataLoader:            geodata.LoaderName(),
		GeositeMatcher:           geodata.SiteMatcherName(),
		TCPConcurrent:            dialer.GetTcpConcurrent(),
		FindProcessMode:          tunnel.FindProcessMode(),
		Sniffing:                 tunnel.IsSniffing(),
		GlobalUA:                 mihomoHttp.UA(),
		ETagSupport:              resource.ETag(),
		KeepAliveInterval:        int(keepalive.KeepAliveInterval() / time.Second),
		KeepAliveIdle:            int(keepalive.KeepAliveIdle() / time.Second),
		DisableKeepAlive:         keepalive.DisableKeepAlive(),
	}
	if applied := CurrentUpdateConfiguration(); applied != nil {
		general.IPTables = applied.General.IPTables
		general.IPTables.Bypass = append([]string(nil), general.IPTables.Bypass...)
	}

	return general
}

func updateListeners(general *config.General, listeners map[string]C.InboundListener, force, wasManaged bool) error {
	listener.PatchInboundListeners(listeners, tunnel.Tunnel, true)
	if !force {
		if general.TransparentProxy.Enable {
			return listener.ReCreateTProxy(general.TProxyPort, tunnel.Tunnel)
		}
		if wasManaged {
			return listener.ReCreateTProxy(listener.GetPorts().TProxyPort, tunnel.Tunnel)
		}
		return nil
	}

	allowLan := general.AllowLan
	listener.SetAllowLan(allowLan)
	inbound.SetSkipAuthPrefixes(general.SkipAuthPrefixes)
	inbound.SetAllowedIPs(general.LanAllowedIPs)
	inbound.SetDisAllowedIPs(general.LanDisAllowedIPs)

	bindAddress := general.BindAddress
	listener.SetBindAddress(bindAddress)
	listener.ReCreateHTTP(general.Port, tunnel.Tunnel)
	listener.ReCreateSocks(general.SocksPort, tunnel.Tunnel)
	listener.ReCreateRedir(general.RedirPort, tunnel.Tunnel)
	err := listener.ReCreateTProxy(general.TProxyPort, tunnel.Tunnel)
	listener.ReCreateMixed(general.MixedPort, tunnel.Tunnel)
	listener.ReCreateShadowSocks(general.ShadowSocksConfig, tunnel.Tunnel)
	listener.ReCreateVmess(general.VmessConfig, tunnel.Tunnel)
	listener.ReCreateTuic(general.TuicServer, tunnel.Tunnel)
	return err
}

func updateTun(general *config.General) error {
	return listener.ReCreateTun(general.Tun, tunnel.Tunnel)
}

func updateExperimental(c *config.Experimental) {
	if c.QUICGoDisableGSO {
		_ = os.Setenv("QUIC_GO_DISABLE_GSO", strconv.FormatBool(true))
	}
	if c.QUICGoDisableECN {
		_ = os.Setenv("QUIC_GO_DISABLE_ECN", strconv.FormatBool(true))
	}
	resolver.SetIP4PEnable(c.IP4PEnable)
}

func updateNTP(c *config.NTP) {
	if c.Enable {
		ntp.ReCreateNTPService(
			net.JoinHostPort(c.Server, strconv.Itoa(c.Port)),
			time.Duration(c.Interval),
			c.DialerProxy,
			tunnel.Tunnel,
			c.WriteToSystem,
		)
	} else {
		ntp.ReCreateNTPService("", 0, "", nil, false)
	}
}

func closeDNSResolvers() {
	// Replaced resolvers must stop proactive refreshes before their routing
	// configuration and client pool become stale. Close is idempotent because
	// these public resolver handles may refer to the same resolver collection.
	if dnsResolverOwner != nil {
		dnsResolverOwner.Close()
		dnsResolverOwner = nil
	}
	for _, old := range []resolver.Resolver{resolver.DefaultResolver, resolver.ProxyServerHostResolver, resolver.DirectHostResolver} {
		if closer, ok := old.(interface{ Close() }); ok {
			closer.Close()
		}
	}
}

func updateDNS(c *config.DNS, generalIPv6 bool) {
	closeDNSResolvers()
	dnsstats.Default.Configure(c.Enable && c.Observability)
	if !c.Enable {
		resolver.DefaultResolver = nil
		resolver.DefaultHostMapper = nil
		resolver.DefaultService = nil
		resolver.ProxyServerHostResolver = nil
		resolver.DirectHostResolver = nil
		if c.DNSRuleRouting {
			// Keep proxy-host bootstrap independent of business DNS, including
			// when the local DNS service is disabled.
			r := dns.NewResolver(dns.Config{
				Default:           c.DefaultNameserver,
				ProxyServer:       c.ProxyServerNameserver,
				ProxyServerPolicy: c.ProxyServerPolicy,
				IPv6:              c.IPv6 && generalIPv6,
				IPv6Timeout:       c.IPv6Timeout,
				CacheAlgorithm:    c.CacheAlgorithm,
				CacheMaxSize:      c.CacheMaxSize,
			})
			dnsResolverOwner = &r
			if r.ProxyResolver.Invalid() {
				resolver.ProxyServerHostResolver = r.ProxyResolver
			} else {
				resolver.ProxyServerHostResolver = r.BootstrapResolver
			}
		}
		dns.ReCreateServer("", nil, nil)
		return
	}

	ipv6 := c.IPv6 && generalIPv6
	r := dns.NewResolver(dns.Config{
		RuleRouting:          c.DNSRuleRouting,
		Main:                 c.NameServer,
		Fallback:             c.Fallback,
		IPv6:                 ipv6,
		IPv6Timeout:          c.IPv6Timeout,
		FallbackIPFilter:     c.FallbackIPFilter,
		FallbackDomainFilter: c.FallbackDomainFilter,
		FallbackLazyQuery:    c.FallbackLazyQuery,
		Default:              c.DefaultNameserver,
		Policy:               c.NameServerPolicy,
		ProxyServer:          c.ProxyServerNameserver,
		ProxyServerPolicy:    c.ProxyServerPolicy,
		DirectServer:         c.DirectNameServer,
		DirectFollowPolicy:   c.DirectFollowPolicy,
		CacheAlgorithm:       c.CacheAlgorithm,
		CacheMaxSize:         c.CacheMaxSize,
		SpeedCheck:           c.SpeedCheck,
		CacheOptions:         c.CacheOptions,
		AnswerPolicy:         c.AnswerPolicy,
		DualStack:            c.DualStack,
	})
	dnsResolverOwner = &r
	m := dns.NewEnhancer(dns.EnhancerConfig{
		IPv6:            ipv6,
		EnhancedMode:    c.EnhancedMode,
		RedirHostFilter: c.RedirHostFilter,
		FakeIPPool:      c.FakeIPPool,
		FakeIPPool6:     c.FakeIPPool6,
		FakeIPSkipper:   c.FakeIPSkipper,
		FakeIPTTL:       c.FakeIPTTL,
		UseHosts:        c.UseHosts,
	})

	// reuse cache of old host mapper
	if old := resolver.DefaultHostMapper; old != nil {
		m.PatchFrom(old.(*dns.ResolverEnhancer))
	}

	s := dns.NewService(r, m)

	resolver.DefaultResolver = r
	resolver.DefaultHostMapper = m
	resolver.DefaultService = s
	resolver.UseSystemHosts = c.UseSystemHosts

	if r.ProxyResolver.Invalid() {
		resolver.ProxyServerHostResolver = r.ProxyResolver
	} else if c.DNSRuleRouting {
		resolver.ProxyServerHostResolver = r.BootstrapResolver
	} else {
		resolver.ProxyServerHostResolver = r.Resolver
	}

	if r.DirectResolver.Invalid() {
		resolver.DirectHostResolver = r.DirectResolver
	} else {
		resolver.DirectHostResolver = r.Resolver
	}

	lc := inbound.NewListenConfig()
	lc.SetRouteMark(c.ListenRoutingMark)
	dns.ReCreateServer(c.Listen, lc, s)
}

func updateHosts(tree *trie.DomainTrie[resolver.HostValue]) {
	resolver.DefaultHosts = resolver.NewHosts(tree)
}

func updateProxies(proxies map[string]C.Proxy, providers map[string]P.ProxyProvider) {
	tunnel.UpdateProxies(proxies, providers)
}

func updateRules(rules []C.Rule, subRules map[string][]C.Rule, ruleProviders map[string]P.RuleProvider) {
	tunnel.UpdateRules(rules, subRules, ruleProviders)
}

func loadProvider[T P.Provider](providers map[string]T) {
	load := func(pv T) {
		name := pv.Name()
		if pv.VehicleType() == P.Compatible {
			log.Infoln("Start initial compatible provider %s", name)
		} else {
			log.Infoln("Start initial provider %s", name)
		}

		if err := pv.Initial(); err != nil {
			switch pv.Type() {
			case P.Proxy:
				{
					log.Errorln("initial proxy provider %s error: %v", name, err)
				}
			case P.Rule:
				{
					log.Errorln("initial rule provider %s error: %v", name, err)
				}
			}
		}
	}

	wg := sync.WaitGroup{}
	ch := make(chan struct{}, concurrentCount)
	for _, pv := range providers {
		pv := pv
		wg.Add(1)
		ch <- struct{}{}
		go func() {
			defer func() { <-ch; wg.Done() }()
			load(pv)
		}()
	}
	wg.Wait()
}

func updateSniffer(snifferConfig *sniffer.Config) {
	dispatcher, err := sniffer.NewDispatcher(snifferConfig)
	if err != nil {
		log.Warnln("initial sniffer failed, err:%v", err)
	}

	tunnel.UpdateSniffer(dispatcher)

	if snifferConfig.Enable {
		log.Infoln("Sniffer is loaded and working")
	} else {
		log.Infoln("Sniffer is closed")
	}
}

func updateTunnels(tunnels []LC.Tunnel) {
	listener.PatchTunnel(tunnels, tunnel.Tunnel)
}

func updateUpdater(cfg *config.Config) func() {
	// Publish cancellation and the new policy at the beginning of ApplyConfig.
	// No new activation/bootstrap can use networking before its reset completes.
	ready := make(chan struct{})
	general := cfg.General

	uiUpdater := updater.NewFixedUIUpdater(general.ExternalUIAutoUpdate, general.ExternalUIUpdateInterval)
	updater.SetUIUpdaterWhenReady(uiUpdater, ready)
	updater.DefaultCoreUpdater.ConfigureAutoUpdateWhenReady(general.CoreAutoUpdate, general.CoreUpdateInterval, ready)
	updater.DefaultGeoUpdater.ConfigureAutoUpdateWhenReady(general.GeoAutoUpdate, general.GeoUpdateInterval, ready)
	// Bootstrap an empty managed directory without holding the reload mutex
	// during a network download. The updater gate rechecks emptiness itself.
	uiUpdater.StartAutoDownloadUI()

	// Called once at the end of the same ApplyConfig transaction. Capture the
	// new resolver instances; never read mutable globals in the goroutines or
	// apply a stale configuration after waiting for their network locks.
	var finish sync.Once
	return func() {
		finish.Do(func() {
			main, system := resolver.DefaultResolver, resolver.SystemResolver
			previous := updaterResetDone
			updaterResetDone = ready
			mainDone := make(chan struct{})
			go func() {
				if main != nil {
					main.ResetConnection()
				}
				close(mainDone)
			}()
			go func() {
				if system != nil {
					system.ResetConnection()
				}
				<-mainDone
				if previous != nil {
					<-previous
				}
				close(ready)
			}()
		})
	}
}

//go:linkname temporaryUpdateGeneral github.com/metacubex/mihomo/config.temporaryUpdateGeneral
func temporaryUpdateGeneral(general *config.General) func() {
	oldGeneral := GetGeneral()
	// Parsing geodata/providers needs transport settings, not a temporary
	// routing policy. Changing mode/process policy here would affect live
	// connections even when the candidate configuration is later rejected.
	updateGeneralSettings(general, false)
	return func() {
		updateGeneralSettings(oldGeneral, false)
	}
}

func updateGeneral(general *config.General, logging bool) {
	tunnel.SetMode(general.Mode)
	tunnel.SetFindProcessMode(general.FindProcessMode)
	updateGeneralSettings(general, logging)
}

func updateGeneralSettings(general *config.General, logging bool) {
	resolver.DisableIPv6 = !general.IPv6

	dialer.SetTcpConcurrent(general.TCPConcurrent)
	if logging && general.TCPConcurrent {
		log.Infoln("Use tcp concurrent")
	}

	inbound.SetTfo(general.InboundTfo)
	inbound.SetMPTCP(general.InboundMPTCP)

	keepalive.SetKeepAliveIdle(time.Duration(general.KeepAliveIdle) * time.Second)
	keepalive.SetKeepAliveInterval(time.Duration(general.KeepAliveInterval) * time.Second)
	keepalive.SetDisableKeepAlive(general.DisableKeepAlive)

	adapter.UnifiedDelay.Store(general.UnifiedDelay)

	dialer.DefaultInterface.Store(general.Interface)
	dialer.DefaultRoutingMark.Store(int32(general.RoutingMark))
	if logging && general.RoutingMark > 0 {
		log.Infoln("Use routing mark: %#x", general.RoutingMark)
	}

	iface.FlushCache()

	geodata.SetGeodataMode(general.GeodataMode)
	geodata.SetLoader(general.GeodataLoader)
	geodata.SetSiteMatcher(general.GeositeMatcher)
	geodata.SetGeoIpUrl(general.GeoXUrl.GeoIp)
	geodata.SetGeoSiteUrl(general.GeoXUrl.GeoSite)
	geodata.SetMmdbUrl(general.GeoXUrl.Mmdb)
	geodata.SetASNUrl(general.GeoXUrl.ASN)
	mihomoHttp.SetUA(general.GlobalUA)
	resource.SetETag(general.ETagSupport)
}

func updateUsers(users []auth.AuthUser) {
	authenticator := auth.NewAuthenticator(users)
	authStore.Default.SetAuthenticator(authenticator)
	if authenticator != nil {
		log.Infoln("Authentication of local server updated")
	}
}

func updateProfile(cfg *config.Config) {
	profileCfg := cfg.Profile

	profile.StoreSelected.Store(profileCfg.StoreSelected)
	if profileCfg.StoreSelected {
		patchSelectGroup(cfg.Proxies)
	}
}

func patchSelectGroup(proxies map[string]C.Proxy) {
	mapping := cachefile.Cache().SelectedMap()
	if mapping == nil {
		return
	}

	for name, outbound := range proxies {
		selector, ok := outbound.Adapter().(outboundgroup.SelectAble)
		if !ok {
			continue
		}

		selected, exist := mapping[name]
		if !exist {
			continue
		}

		selector.ForceSet(selected)
	}
}

func updateIPTables(cfg *config.Config) {
	tproxy.CleanupTProxyIPTables()

	iptables := cfg.IPTables
	if runtime.GOOS != "linux" || !iptables.Enable {
		return
	}

	var err error
	defer func() {
		if err != nil {
			log.Errorln("[IPTABLES] setting iptables failed: %s", err.Error())
			os.Exit(2)
		}
	}()

	if cfg.General.Tun.Enable {
		err = fmt.Errorf("when tun is enabled, iptables cannot be set automatically")
		return
	}

	var (
		inboundInterface = "lo"
		bypass           = iptables.Bypass
		tProxyPort       = cfg.General.TProxyPort
		dnsCfg           = cfg.DNS
		DnsRedirect      = iptables.DnsRedirect

		dnsPort netip.AddrPort
	)

	if tProxyPort == 0 {
		err = fmt.Errorf("tproxy-port must be greater than zero")
		return
	}

	if DnsRedirect {
		if !dnsCfg.Enable {
			err = fmt.Errorf("DNS server must be enable")
			return
		}

		dnsPort, err = netip.ParseAddrPort(dnsCfg.Listen)
		if err != nil {
			err = fmt.Errorf("DNS server must be correct")
			return
		}
	}

	if iptables.InboundInterface != "" {
		inboundInterface = iptables.InboundInterface
	}

	dialer.DefaultRoutingMark.CompareAndSwap(0, 2158)

	err = tproxy.SetTProxyIPTables(inboundInterface, bypass, uint16(tProxyPort), DnsRedirect, dnsPort.Port())
	if err != nil {
		return
	}

	log.Infoln("[IPTABLES] Setting iptables completed")
}

func Shutdown() error {
	RequestShutdown()
	mux.Lock()
	defer mux.Unlock()
	// Keep admission closed across cleanup retries and post-down execution.
	// Releasing mux must never allow a queued controller request to recreate
	// the TUN or its capture policy after graceful cleanup has completed.
	return shutdownLocked()
}

func shutdownLocked() error {
	// An explicit stop removes capture before any managed backend disappears.
	if err := closeTransparentProxy(); err != nil {
		log.Errorln("Preserve running listeners: stop transparent-proxy: %s", err)
		return err
	}
	return shutdownBackendsLocked()
}

// A process replacement retains capture and blackhole protection until the
// next process verifies the owned kernel resources. Normal shutdown must not
// use this path, because stopping the service should release its interception.
func shutdownForRestartLocked() error {
	if err := prepareTransparentRestart(); err != nil {
		log.Errorln("Preserve running listeners: prepare transparent-proxy restart: %s", err)
		return err
	}
	err := cleanupTunListener()
	if err == nil {
		// Original sysctl values live only in this process. Restore them after
		// TUN close and before reexec, so the replacement samples a fresh base.
		err = completeTransparentRestart()
	}
	if err != nil {
		// The manager still owns its observer and namespace lease. Publish
		// only the listeners that survived cleanup; missing ones recover
		// through its existing backend repair requests.
		return errors.Join(err, abortTransparentRestart(listener.TransparentProxyBackends()))
	}
	shutdownBackgroundLocked()
	return nil
}

func shutdownBackendsLocked() error {
	if err := cleanupTunListener(); err != nil {
		log.Errorln("Close TUN listener: %s", err)
		return err
	}
	shutdownBackgroundLocked()
	return nil
}

func shutdownBackgroundLocked() {
	invalidateUpdateConfiguration()
	updater.StopAutomaticUpdates()
	tproxy.CleanupTProxyIPTables()
	resolver.StoreFakePoolState()
	closeDNSResolvers()

	log.Warnln("Mihomo shutting down")
}
