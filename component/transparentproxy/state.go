package transparentproxy

import (
	"fmt"
	"net/netip"
	"os/user"
	"strconv"

	"golang.org/x/exp/slices"
)

// These retain the established exclusions. Fake-IP is evaluated before this
// set by the firewall, including Fake-IP inside an otherwise reserved prefix.
var reservedPrefixes = parsePrefixes([]string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
	"::/127", "::ffff:0:0/96", "::ffff:0:0:0/96", "64:ff9b::/96", "100::/64", "2001::/32", "2001:20::/28",
	"2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
})

func desiredState(cfg Config, runtime Runtime, network networkState, uids, marks []uint32) ([]routePolicy, firewallState) {
	state := firewallState{
		Enabled: cfg.Enable, TCP: cfg.TCP, UDP: cfg.UDP, IPv6: runtime.IPv6,
		TProxyMark: cfg.Routing.TProxy.Mark, TunMark: cfg.Routing.TUN.Mark,
		TProxyPort: runtime.TProxyPort, TunName: runtime.TunName,
		TProxyTCP4: runtime.TProxyPort != 0 && runtime.TProxyTCP,
		TProxyTCP6: runtime.TProxyPort != 0 && runtime.TProxyTCPIPv6 && runtime.IPv6 && network.IPv6Available,
		TProxyUDP4: runtime.TProxyPort != 0 && runtime.TProxyUDP,
		TProxyUDP6: runtime.TProxyPort != 0 && runtime.TProxyUDPIPv6 && runtime.IPv6 && network.IPv6Available,
		BypassUIDs: slices.Clone(uids), BypassMarks: slices.Clone(marks),
		BypassUDPPorts: slices.Clone(cfg.Local.BypassUDPPorts),
		FakeIP:         normalizePrefixes(runtime.FakeIP),
	}
	if cfg.Local.Enable {
		state.Local4 = resolveInterfaces(cfg.Local.Interfaces, network.DefaultIPv4, network, runtime.TunName)
		state.Local6 = resolveInterfaces(cfg.Local.Interfaces, network.DefaultIPv6, network, runtime.TunName)
	}
	if cfg.Gateway.Enable {
		state.Gateway = resolveInterfaces(cfg.Gateway.Interfaces, network.DefaultIPv4, network, runtime.TunName)
		state.Gateway = append(state.Gateway, resolveInterfaces(cfg.Gateway.Interfaces, network.DefaultIPv6, network, runtime.TunName)...)
		state.Gateway = uniqueStrings(state.Gateway)
	}
	if cfg.Bypass.ReservedAddresses {
		state.Bypass = slices.Clone(reservedPrefixes)
	}
	state.Bypass = append(state.Bypass, parsePrefixes(cfg.Bypass.ExtraAddresses)...)
	prefixInterfaces := resolveInterfaces(cfg.Bypass.InterfacePrefixes, network.DefaultIPv4, network, runtime.TunName)
	prefixInterfaces = append(prefixInterfaces, resolveInterfaces(cfg.Bypass.InterfacePrefixes, network.DefaultIPv6, network, runtime.TunName)...)
	loopbackIndex := 0
	for _, iface := range network.Interfaces {
		if iface.Up && iface.Loopback {
			loopbackIndex = iface.Index
		}
		if iface.Name == runtime.TunName && iface.Index == runtime.TunIndex && iface.Up && runtime.TunReady {
			for _, address := range iface.Addresses {
				if !address.IsValid() || address.Addr().IsUnspecified() {
					continue
				}
				if address.Addr().Is4() {
					state.Tun4 = true
				} else if runtime.TunIPv6Ready && runtime.IPv6 && network.IPv6Available {
					state.Tun6 = true
				}
			}
		}
		if !iface.Up || iface.Loopback || iface.Name == runtime.TunName {
			continue
		}
		if !slices.Contains(prefixInterfaces, "*") && !slices.Contains(prefixInterfaces, iface.Name) {
			continue
		}
		for _, address := range iface.Addresses {
			if address.IsValid() && address.Bits() > 0 && !address.Addr().IsUnspecified() {
				state.Bypass = append(state.Bypass, address.Masked())
			}
		}
	}
	state.Bypass = normalizePrefixes(state.Bypass)
	var policies []routePolicy
	for _, ipv6 := range []bool{false, true} {
		if ipv6 && !runtime.IPv6 {
			continue
		}
		if cfg.Uses(ModeTProxy) {
			ready := cfg.TCP == ModeTProxy && state.TProxyTCP4 || cfg.UDP == ModeTProxy && state.TProxyUDP4
			if ipv6 {
				ready = cfg.TCP == ModeTProxy && state.TProxyTCP6 || cfg.UDP == ModeTProxy && state.TProxyUDP6
			}
			p := cfg.Routing.TProxy
			policies = append(policies, routePolicy{Backend: ModeTProxy, Mark: p.Mark, Table: p.Table, Priority: p.Priority, IPv6: ipv6, IfIndex: loopbackIndex, Ready: ready && loopbackIndex != 0})
		}
		if cfg.Uses(ModeTUN) {
			p := cfg.Routing.TUN
			ready := state.Tun4
			if ipv6 {
				ready = state.Tun6
			}
			policies = append(policies, routePolicy{Backend: ModeTUN, Mark: p.Mark, Table: p.Table, Priority: p.Priority, IPv6: ipv6, IfIndex: runtime.TunIndex, Ready: ready})
		}
	}
	return policies, state
}

func needsBackendRepair(cfg Config, runtime Runtime, network networkState) bool {
	if !cfg.Enable {
		return false
	}
	if cfg.Uses(ModeTUN) {
		found := false
		ipv4Address := false
		ipv6Address := false
		for _, iface := range network.Interfaces {
			if iface.Name != runtime.TunName || iface.Index != runtime.TunIndex || !iface.Up {
				continue
			}
			found = true
			for _, address := range iface.Addresses {
				if !address.IsValid() || address.Addr().IsUnspecified() {
					continue
				}
				if address.Addr().Is4() {
					ipv4Address = true
				} else {
					ipv6Address = true
				}
			}
		}
		if !runtime.TunReady || !found || !ipv4Address {
			return true
		}
		if runtime.IPv6 && runtime.TunIPv6Requested && network.IPv6Available && (!runtime.TunIPv6Ready || !ipv6Address) {
			return true
		}
	}
	if cfg.TCP == ModeTProxy && (!runtime.TProxyTCP || runtime.IPv6 && network.IPv6Available && !runtime.TProxyTCPIPv6) {
		return true
	}
	if cfg.UDP == ModeTProxy && (!runtime.TProxyUDP || runtime.IPv6 && network.IPv6Available && !runtime.TProxyUDPIPv6) {
		return true
	}
	return false
}

// "auto" is resolved separately for each address family. Missing links stay
// absent until a system event announces them; no first-interface guess is used.
func resolveInterfaces(names []string, outlet string, network networkState, tunName string) []string {
	var result []string
	for _, name := range names {
		if name == "*" {
			return []string{"*"}
		}
		if name == "auto" {
			name = outlet
		}
		if name == "" || name == tunName {
			continue
		}
		for _, iface := range network.Interfaces {
			if iface.Name == name && iface.Up && !iface.Loopback {
				result = append(result, name)
				break
			}
		}
	}
	return uniqueStrings(result)
}

func resolveBypassUsers(names []string) ([]uint32, error) {
	var uids []uint32
	for _, name := range names {
		value, err := strconv.ParseUint(name, 10, 32)
		if err != nil {
			account, lookupErr := user.Lookup(name)
			if lookupErr != nil {
				return nil, fmt.Errorf("transparent-proxy bypass user %q: %w", name, lookupErr)
			}
			value, err = strconv.ParseUint(account.Uid, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("transparent-proxy bypass user %q has invalid UID", name)
			}
		}
		uids = append(uids, uint32(value))
	}
	slices.Sort(uids)
	return slices.Compact(uids), nil
}

func uniqueStrings(values []string) []string { slices.Sort(values); return slices.Compact(values) }

func parsePrefixes(values []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	return prefixes
}

func normalizePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix.IsValid() {
			result = append(result, prefix.Masked())
		}
	}
	slices.SortFunc(result, func(a, b netip.Prefix) int {
		if order := a.Addr().Compare(b.Addr()); order != 0 {
			return order
		}
		return a.Bits() - b.Bits()
	})
	return slices.Compact(result)
}
