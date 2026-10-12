package provider

import (
	"strings"
	"testing"
)

func TestProviderDNSStatsIdentityIncludesAppliedOverrides(t *testing.T) {
	const payload = `proxies:
  - {name: same-name, type: socks5, server: 192.0.2.20, port: 1080, password: provider-secret, udp: true}
`
	identity := func(name, config string, override overrideSchema) string {
		t.Helper()
		parser, err := NewProxiesParser(name, nil, "", "", "", "", override, "")
		if err != nil {
			t.Fatal(err)
		}
		proxies, err := parser([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		if len(proxies) != 1 {
			t.Fatalf("provider produced %d nodes", len(proxies))
		}
		t.Cleanup(func() { _ = proxies[0].Close() })
		source, ok := proxies[0].Adapter().(interface{ DNSStatsIdentity() string })
		if !ok || source.DNSStatsIdentity() == "" {
			t.Fatal("provider node has no stable statistics identity")
		}
		return source.DNSStatsIdentity()
	}
	mark := 47
	first := identity("subscription", payload, overrideSchema{RoutingMark: &mark})
	if identity("subscription", payload, overrideSchema{RoutingMark: &mark}) != first {
		t.Fatal("same provider refresh or reconstruction split statistics")
	}
	mark = 48
	if identity("subscription", payload, overrideSchema{RoutingMark: &mark}) == first {
		t.Fatal("provider override was omitted from the configuration fingerprint")
	}
	mark = 47
	if identity("other-subscription", payload, overrideSchema{RoutingMark: &mark}) == first {
		t.Fatal("different providers were merged")
	}
	if identity("subscription", strings.Replace(payload, "provider-secret", "updated-secret", 1), overrideSchema{RoutingMark: &mark}) == first {
		t.Fatal("changed provider credentials were merged")
	}
}
