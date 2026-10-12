package transparentproxy

import (
	"net/netip"
	"testing"

	"golang.org/x/exp/slices"
)

func testNetwork() networkState {
	return networkState{IPv6Available: true, DefaultIPv4: "eth0", DefaultIPv6: "eth1", Interfaces: []networkInterface{
		{Index: 1, Name: "lo", Up: true, Loopback: true, Addresses: parsePrefixes([]string{"127.0.0.1/8", "::1/128"})},
		{Index: 2, Name: "eth0", Up: true, Addresses: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24"), netip.MustParsePrefix("240e:1234:5678:90ab::5/64")}},
		{Index: 3, Name: "eth1", Up: true, Addresses: []netip.Prefix{netip.MustParsePrefix("240e:1234:5678:abcd::5/64")}},
		{Index: 4, Name: "utun", Up: true, Addresses: []netip.Prefix{netip.MustParsePrefix("198.18.0.1/30"), netip.MustParsePrefix("fdfe:dcba:9876::1/126")}},
	}}
}

func testRuntime() Runtime {
	return Runtime{IPv6: true, TunName: "utun", TunIndex: 4, TunReady: true, TunIPv6Ready: true, TunIPv6Requested: true,
		TProxyPort: 7895, TProxyTCP: true, TProxyUDP: true, TProxyTCPIPv6: true, TProxyUDPIPv6: true,
		FakeIP: parsePrefixes([]string{"198.18.0.0/16", "fdfe:dcba:9876::/64"})}
}

func TestDesiredStateUsesPerFamilyOutletAndRealPrefixes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable = true
	policies, state := desiredState(cfg, testRuntime(), testNetwork(), nil, []uint32{DefaultOutboundMark})
	if len(policies) != 4 || !state.Tun4 || !state.Tun6 {
		t.Fatalf("dual stack backend not ready: %+v %+v", policies, state)
	}
	if !slices.Equal(state.Local4, []string{"eth0"}) || !slices.Equal(state.Local6, []string{"eth1"}) {
		t.Fatalf("family outlets collapsed: %v %v", state.Local4, state.Local6)
	}
	for _, prefix := range []string{"10.0.0.0/24", "240e:1234:5678:90ab::/64", "240e:1234:5678:abcd::/64"} {
		if !slices.Contains(state.Bypass, netip.MustParsePrefix(prefix)) {
			t.Fatalf("real interface prefix %s not learned", prefix)
		}
	}
	if slices.Contains(state.Bypass, netip.MustParsePrefix("198.18.0.0/30")) || slices.Contains(state.Bypass, netip.MustParsePrefix("fdfe:dcba:9876::/126")) {
		t.Fatal("TUN's own prefixes were learned as bypasses")
	}
}

func TestDesiredStateDoesNotAdoptReusedTunName(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable = true
	runtime := testRuntime()
	runtime.TunIndex = 99
	policies, state := desiredState(cfg, runtime, testNetwork(), nil, nil)
	if state.Tun4 || state.Tun6 {
		t.Fatal("a different interface reused the old TUN name and was trusted")
	}
	for _, policy := range policies {
		if policy.Backend == ModeTUN && policy.Ready {
			t.Fatal("installed forwarding to unrelated interface")
		}
	}
}

func TestIPv6IntentRemainsClosedUntilActualBackendsExist(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable = true
	runtime := testRuntime()
	network := testNetwork()
	network.IPv6Available = false
	policies, state := desiredState(cfg, runtime, network, nil, nil)
	if !state.IPv6 || state.Tun6 || state.TProxyTCP6 || len(policies) != 4 {
		t.Fatalf("disabled stack lost policy intent or was forwarded: %+v", state)
	}
	for _, policy := range policies {
		if policy.IPv6 && policy.Ready {
			t.Fatal("IPv6 ready while stack unavailable")
		}
	}
	network.IPv6Available = true
	runtime.TunIPv6Ready, runtime.TProxyTCPIPv6 = false, false
	_, state = desiredState(cfg, runtime, network, nil, nil)
	if state.Tun6 || state.TProxyTCP6 || !state.Tun4 || !state.TProxyTCP4 {
		t.Fatal("IPv6 recovery inferred from namespace instead of actual listeners")
	}
	if !needsBackendRepair(cfg, runtime, network) {
		t.Fatal("missing family did not request listener repair")
	}
}

func TestAddressRemovalAndDisabledFamilyDoNotLeaveStalePrefixes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable = true
	cfg.Bypass.ReservedAddresses = false
	network := testNetwork()
	network.Interfaces[1].Addresses = nil
	runtime := testRuntime()
	runtime.IPv6 = false
	policies, state := desiredState(cfg, runtime, network, nil, nil)
	if slices.Contains(state.Bypass, netip.MustParsePrefix("240e:1234:5678:90ab::/64")) {
		t.Fatal("removed address remained bypassed")
	}
	if state.IPv6 || len(policies) != 2 {
		t.Fatal("explicit IPv6 disable did not remove the family policy")
	}
}

func TestRemovedTunAddressesBlockAndRequestRepair(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable = true
	for _, family := range []string{"ipv4", "ipv6"} {
		t.Run(family, func(t *testing.T) {
			network := testNetwork()
			tun := &network.Interfaces[3]
			if family == "ipv4" {
				tun.Addresses = tun.Addresses[1:]
			} else {
				tun.Addresses = tun.Addresses[:1]
			}
			_, state := desiredState(cfg, testRuntime(), network, nil, nil)
			if family == "ipv4" && state.Tun4 || family == "ipv6" && state.Tun6 {
				t.Fatal("missing address family was still forwarded")
			}
			if !needsBackendRepair(cfg, testRuntime(), network) {
				t.Fatal("missing address family did not request listener repair")
			}
		})
	}
}

func TestExplicitlyUnrequestedTunIPv6RemainsClosedWithoutRetries(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enable = true
	runtime := testRuntime()
	runtime.TunIPv6Requested, runtime.TunIPv6Ready = false, false
	network := testNetwork()
	network.Interfaces[3].Addresses = network.Interfaces[3].Addresses[:1]
	_, state := desiredState(cfg, runtime, network, nil, nil)
	if state.Tun6 || !state.Tun4 {
		t.Fatal("unrequested IPv6 did not preserve the intended per-family state")
	}
	if needsBackendRepair(cfg, runtime, network) {
		t.Fatal("explicitly empty TUN IPv6 configuration caused perpetual repair")
	}
}
