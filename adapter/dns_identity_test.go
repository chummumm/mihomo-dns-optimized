package adapter

import (
	"strings"
	"testing"

	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/dialer"
	C "github.com/metacubex/mihomo/constant"

	"go.yaml.in/yaml/v3"
)

const dnsIdentityProxyYAML = `
name: same-name
type: socks5
server: 192.0.2.10
port: 1080
username: test-user
password: test-secret
udp: true
smux:
  enabled: true
  protocol: smux
  max-connections: 2
  padding: false
`

func parseDNSIdentityProxy(t *testing.T, config string, options ...ProxyOption) (C.Proxy, map[string]any) {
	t.Helper()
	var mapping map[string]any
	if err := yaml.Unmarshal([]byte(config), &mapping); err != nil {
		t.Fatal(err)
	}
	proxy, err := ParseProxy(mapping, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy, mapping
}

func parsedDNSIdentity(t *testing.T, proxy C.Proxy) string {
	t.Helper()
	source, ok := proxy.Adapter().(interface{ DNSStatsIdentity() string })
	if !ok {
		t.Fatal("parsed lifetime wrapper lost the DNS statistics identity")
	}
	return source.DNSStatsIdentity()
}

func TestParsedDNSStatsIdentitySurvivesReconstructionAndMapOrder(t *testing.T) {
	first, mapping := parseDNSIdentityProxy(t, dnsIdentityProxyYAML, WithProviderName("subscription"))
	want := parsedDNSIdentity(t, first)
	if want == "" || strings.Contains(want, "test-secret") || strings.Contains(want, "test-user") {
		t.Fatal("configuration identity is absent or contains credentials")
	}
	// Both top-level and nested map order differ, as can happen when providers
	// render the same configuration again. Values and effective options match.
	second, _ := parseDNSIdentityProxy(t, `
smux: {padding: false, max-connections: 2, protocol: smux, enabled: true}
password: test-secret
udp: true
port: 1080
server: 192.0.2.10
type: socks5
username: test-user
name: same-name
`, WithProviderName("subscription"))
	if first.Adapter() == second.Adapter() {
		t.Fatal("regression must reconstruct separate live adapters")
	}
	if parsedDNSIdentity(t, second) != want {
		t.Fatal("same reconstructed configuration split statistics")
	}
	// The parser's caller can reuse its map for a later provider update; the
	// already published adapter must retain its construction-time identity.
	mapping["password"] = "replacement-secret"
	mapping["smux"].(map[string]any)["padding"] = true
	if parsedDNSIdentity(t, first) != want {
		t.Fatal("published identity retained a mutable configuration map")
	}
	replacement, err := ParseProxy(mapping, WithProviderName("subscription"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	if parsedDNSIdentity(t, replacement) == want {
		t.Fatal("changed provider configuration inherited the old identity")
	}
}

func TestParsedDNSStatsIdentityKeepsHiddenConfigurationBoundaries(t *testing.T) {
	first, _ := parseDNSIdentityProxy(t, dnsIdentityProxyYAML, WithProviderName("subscription"))
	want := parsedDNSIdentity(t, first)
	for _, tc := range []struct {
		name, old, replacement string
	}{
		{"server", "192.0.2.10", "192.0.2.11"},
		{"port", "port: 1080", "port: 1081"},
		{"username", "test-user", "another-user"},
		{"password", "test-secret", "another-secret"},
		{"mux-options", "padding: false", "padding: true"},
		{"socket-options", "udp: true", "udp: true\nrouting-mark: 47"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, _ := parseDNSIdentityProxy(t, strings.Replace(dnsIdentityProxyYAML, tc.old, tc.replacement, 1), WithProviderName("subscription"))
			if changed.Name() != first.Name() || parsedDNSIdentity(t, changed) == want {
				t.Fatal("same display name merged different outbound configurations")
			}
		})
	}
	otherProvider, _ := parseDNSIdentityProxy(t, dnsIdentityProxyYAML, WithProviderName("another-subscription"))
	if parsedDNSIdentity(t, otherProvider) == want {
		t.Fatal("different provider sources lost their identity boundary")
	}
}

func TestParsedDNSStatsIdentityKeepsOpaqueAPIConfigurationInInstanceScope(t *testing.T) {
	proxy, _ := parseDNSIdentityProxy(t, dnsIdentityProxyYAML, WithDialerForAPI(dialer.NewDialer()))
	if parsedDNSIdentity(t, proxy) != "" {
		t.Fatal("opaque API dialer acquired a misleading stable identity")
	}
	_, mapping := parseDNSIdentityProxy(t, dnsIdentityProxyYAML)
	mapping["api-extension"] = make(chan struct{})
	custom, err := ParseProxy(mapping)
	if err != nil {
		t.Fatalf("statistics fingerprint rejected an otherwise accepted API configuration: %v", err)
	}
	t.Cleanup(func() { _ = custom.Close() })
	if parsedDNSIdentity(t, custom) != "" {
		t.Fatal("unserializable configuration was partially fingerprinted")
	}
}

func TestParsedDirectDNSStatsIdentityMatchesConcreteLookupAdapter(t *testing.T) {
	proxy, _ := parseDNSIdentityProxy(t, "name: local-exit\ntype: direct\ninterface-name: test-iface\nrouting-mark: 42\nip-version: ipv6-prefer\n", WithProviderName("local"))
	direct := outbound.NewDirectWithOption(outbound.DirectOption{
		Name:        "local-exit",
		BasicOption: outbound.BasicOption{ProviderName: "local", Interface: "test-iface", RoutingMark: 42, IPVersion: C.IPv6Prefer},
	})
	if got := parsedDNSIdentity(t, proxy); got == "" || got != direct.DNSStatsIdentity() {
		t.Fatal("parsed DIRECT wrapper and concrete lookup adapter split statistics")
	}
}
