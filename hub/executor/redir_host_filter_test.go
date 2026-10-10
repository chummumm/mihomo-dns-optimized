package executor

import (
	"net/netip"
	"testing"

	"github.com/metacubex/mihomo/component/resolver"
)

func TestRedirHostFilterReloadUpdatesMappingPolicy(t *testing.T) {
	oldOwner := dnsResolverOwner
	oldDefault, oldMapper, oldService := resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService
	oldProxy, oldDirect, oldHosts := resolver.ProxyServerHostResolver, resolver.DirectHostResolver, resolver.UseSystemHosts
	dnsResolverOwner = nil
	resolver.DefaultResolver, resolver.ProxyServerHostResolver, resolver.DirectHostResolver = nil, nil, nil
	resolver.DefaultHostMapper = nil
	t.Cleanup(func() {
		closeDNSResolvers()
		dnsResolverOwner = oldOwner
		resolver.DefaultResolver, resolver.DefaultHostMapper, resolver.DefaultService = oldDefault, oldMapper, oldService
		resolver.ProxyServerHostResolver, resolver.DirectHostResolver, resolver.UseSystemHosts = oldProxy, oldDirect, oldHosts
	})

	const input = `dns:
  enable: true
  enhanced-mode: redir-host
  nameserver: [127.0.0.1:6053]
  default-nameserver: [127.0.0.1:6053]
`
	apply := func(filter string) {
		t.Helper()
		cfg, err := ParseWithBytes([]byte(input + filter))
		if err != nil {
			t.Fatal(err)
		}
		// No Exchange call is made: this exercises the reload wiring without
		// contacting the configured resolver or opening a DNS listener.
		updateDNS(cfg.DNS, false)
	}
	blockedIP, allowedIP := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	assertMapping := func(ip netip.Addr, want string, wantFound bool) {
		t.Helper()
		if host, found := resolver.DefaultHostMapper.FindHostByIP(ip); found != wantFound || (found && host != want) {
			t.Fatalf("mapping for %s = (%q, %v), want (%q, %v)", ip, host, found, want, wantFound)
		}
	}

	apply("")
	resolver.DefaultHostMapper.InsertHostByIP(blockedIP, "blocked.example")
	resolver.DefaultHostMapper.InsertHostByIP(allowedIP, "allowed.example")
	assertMapping(blockedIP, "blocked.example", true)
	assertMapping(allowedIP, "allowed.example", true)

	apply("  redir-host-filter: ['BLOCKED.EXAMPLE.']\n")
	assertMapping(blockedIP, "", false)
	assertMapping(allowedIP, "allowed.example", true)
	resolver.DefaultHostMapper.InsertHostByIP(blockedIP, "blocked.example")
	assertMapping(blockedIP, "", false)

	apply("  redir-host-filter: []\n")
	assertMapping(blockedIP, "", false)
	assertMapping(allowedIP, "allowed.example", true)
	resolver.DefaultHostMapper.InsertHostByIP(blockedIP, "blocked.example")
	assertMapping(blockedIP, "blocked.example", true)
}
