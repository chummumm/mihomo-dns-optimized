package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/metacubex/mihomo/component/process"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/config"
	"github.com/metacubex/mihomo/dns"
	"github.com/metacubex/mihomo/tunnel"

	D "github.com/miekg/dns"
)

func TestDNSRuleRoutingParsingDoesNotToggleLivePolicy(t *testing.T) {
	previous := tunnel.DNSRuleRoutingEnabled()
	defer tunnel.SetDNSRuleRouting(previous)
	tunnel.SetDNSRuleRouting(false)
	if _, err := ParseWithBytes([]byte("dns-rule-routing: true\n")); err != nil {
		t.Fatal(err)
	}
	if tunnel.DNSRuleRoutingEnabled() {
		t.Fatal("merely parsing a candidate config enabled DNS rule routing")
	}
}

func TestDNSRuleRoutingTemporaryGeneralPreservesLiveRoutingPolicy(t *testing.T) {
	oldFlag, oldMode, oldProcess := tunnel.DNSRuleRoutingEnabled(), tunnel.Mode(), tunnel.FindProcessMode()
	t.Cleanup(func() {
		tunnel.SetDNSRuleRouting(oldFlag)
		tunnel.SetMode(oldMode)
		tunnel.SetFindProcessMode(oldProcess)
	})
	tunnel.SetDNSRuleRouting(true)
	tunnel.SetMode(tunnel.Rule)
	tunnel.SetFindProcessMode(process.FindProcessStrict)
	for _, mode := range []tunnel.TunnelMode{tunnel.Global, tunnel.Direct} {
		candidate := GetGeneral()
		candidate.DNSRuleRouting = false
		candidate.Mode = mode
		candidate.FindProcessMode = process.FindProcessOff
		restore := temporaryUpdateGeneral(candidate)
		if !tunnel.DNSRuleRoutingEnabled() || tunnel.Mode() != tunnel.Rule || tunnel.FindProcessMode() != process.FindProcessStrict {
			t.Error("parsing candidate temporarily changed live DNS routing eligibility")
		}
		// A real API policy change made during parsing must also survive its
		// cleanup; restoring temporary transport settings is not a rollback.
		tunnel.SetMode(tunnel.Direct)
		tunnel.SetFindProcessMode(process.FindProcessAlways)
		restore()
		if tunnel.Mode() != tunnel.Direct || tunnel.FindProcessMode() != process.FindProcessAlways {
			t.Error("temporary parsing cleanup overwrote a live policy change")
		}
		tunnel.SetMode(tunnel.Rule)
		tunnel.SetFindProcessMode(process.FindProcessStrict)
	}
}

func TestDNSRuleRoutingBootstrapNeverFallsBackToBusinessResolver(t *testing.T) {
	oldOwner := dnsResolverOwner
	dnsResolverOwner = nil
	oldDefault, oldMapper, oldService := resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService
	oldProxy, oldDirect, oldHosts := resolver.ProxyServerHostResolver, resolver.DirectHostResolver, resolver.UseSystemHosts
	resolver.DefaultResolver, resolver.ProxyServerHostResolver, resolver.DirectHostResolver = nil, nil, nil
	t.Cleanup(func() {
		closeDNSResolvers()
		dnsResolverOwner = oldOwner
		resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService = oldDefault, oldMapper, oldService
		resolver.ProxyServerHostResolver, resolver.DirectHostResolver, resolver.UseSystemHosts = oldProxy, oldDirect, oldHosts
	})
	for _, enabled := range []bool{true, false} {
		updateDNS(&config.DNS{
			Enable:            true,
			DNSRuleRouting:    enabled,
			NameServer:        []dns.NameServer{{Net: "udp", Addr: "127.0.0.1:6053"}},
			DefaultNameserver: []dns.NameServer{{Net: "udp", Addr: "9.9.9.9:53"}},
		}, false)
		if resolver.ProxyServerHostResolver == nil {
			t.Fatal("bootstrap resolver is missing")
		}
		main := resolver.DefaultResolver.(dns.Resolvers).Resolver
		if (resolver.ProxyServerHostResolver == main) == enabled {
			t.Fatalf("wrong proxy fallback: flag=%v bootstrap equals main=%v", enabled, resolver.ProxyServerHostResolver == main)
		}
	}
}

func TestDNSRuleRoutingBootstrapWithoutLocalDNSService(t *testing.T) {
	oldOwner := dnsResolverOwner
	dnsResolverOwner = nil
	oldDefault, oldMapper, oldService := resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService
	oldProxy, oldDirect := resolver.ProxyServerHostResolver, resolver.DirectHostResolver
	resolver.DefaultResolver, resolver.ProxyServerHostResolver, resolver.DirectHostResolver = nil, nil, nil
	t.Cleanup(func() {
		closeDNSResolvers()
		dnsResolverOwner = oldOwner
		resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService = oldDefault, oldMapper, oldService
		resolver.ProxyServerHostResolver, resolver.DirectHostResolver = oldProxy, oldDirect
	})
	for _, proxyServers := range [][]dns.NameServer{nil, {{Net: "udp", Addr: "9.9.9.10:53"}}} {
		for _, enabled := range []bool{true, false} {
			updateDNS(&config.DNS{
				Enable:                false,
				DNSRuleRouting:        enabled,
				DefaultNameserver:     []dns.NameServer{{Net: "udp", Addr: "9.9.9.9:53"}},
				ProxyServerNameserver: proxyServers,
			}, false)
			if resolver.DefaultResolver != nil || resolver.DefaultService != nil || resolver.DefaultHostMapper != nil || resolver.DirectHostResolver != nil {
				t.Fatal("bootstrap setup unexpectedly enabled the local DNS service")
			}
			if enabled {
				if resolver.ProxyServerHostResolver == nil || !resolver.ProxyServerHostResolver.Invalid() {
					t.Fatal("proxy-host resolution has no independent bootstrap while local DNS is off")
				}
			} else if resolver.ProxyServerHostResolver != nil {
				t.Fatal("feature-off behavior changed for disabled DNS")
			}
		}
	}
}

func TestCacheControlDisabledDNSClosesUnexposedBootstrap(t *testing.T) {
	oldOwner := dnsResolverOwner
	oldDefault, oldMapper, oldService := resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService
	oldProxy, oldDirect := resolver.ProxyServerHostResolver, resolver.DirectHostResolver
	dnsResolverOwner = nil
	resolver.DefaultResolver, resolver.ProxyServerHostResolver, resolver.DirectHostResolver = nil, nil, nil
	t.Cleanup(func() {
		closeDNSResolvers()
		dnsResolverOwner = oldOwner
		resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService = oldDefault, oldMapper, oldService
		resolver.ProxyServerHostResolver, resolver.DirectHostResolver = oldProxy, oldDirect
	})
	updateDNS(&config.DNS{
		DNSRuleRouting:        true,
		ProxyServerNameserver: []dns.NameServer{{Net: "udp", Addr: "127.0.0.1:53"}},
	}, false)
	if dnsResolverOwner == nil || resolver.ProxyServerHostResolver == dnsResolverOwner.BootstrapResolver {
		t.Fatal("fixture did not create a privately owned bootstrap resolver")
	}
	bootstrap := dnsResolverOwner.BootstrapResolver
	updateDNS(&config.DNS{}, false)
	query := new(D.Msg)
	query.SetQuestion("bootstrap.example.", D.TypeA)
	// The empty bootstrap pool cannot contact the network even if Close is
	// broken. Cancellation specifically proves that its owner was closed.
	if _, err := bootstrap.ExchangeContext(context.Background(), query); !errors.Is(err, context.Canceled) {
		t.Fatalf("disabled DNS left its unexposed bootstrap resolver open: %v", err)
	}
	if dnsResolverOwner != nil {
		t.Fatal("disabled DNS retained its replaced resolver owner")
	}
}
