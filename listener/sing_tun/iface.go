package sing_tun

import (
	"fmt"
	"net"
	"net/netip"
	"sort"

	"github.com/metacubex/mihomo/component/iface"
	"github.com/metacubex/mihomo/component/iface/anet"

	"github.com/metacubex/sing/common/control"
)

type defaultInterfaceFinder struct{}

var DefaultInterfaceFinder control.InterfaceFinder = (*defaultInterfaceFinder)(nil)

func (f *defaultInterfaceFinder) Update() error {
	iface.FlushCache()
	_, err := iface.Interfaces()
	return err
}

func (f *defaultInterfaceFinder) Interfaces() []control.Interface {
	ifaces, err := iface.Interfaces()
	if err != nil {
		return nil
	}
	interfaces := make([]control.Interface, 0, len(ifaces))
	for _, _interface := range ifaces {
		interfaces = append(interfaces, control.Interface(*_interface))
	}

	return interfaces
}

func (f *defaultInterfaceFinder) ByName(name string) (*control.Interface, error) {
	netInterface, err := iface.ResolveInterface(name)
	if err == nil {
		return readDefaultInterface(control.Interface(*netInterface).NetInterface())
	}
	if netInterface, lookupErr := net.InterfaceByName(name); lookupErr == nil {
		// The shared cache omits interfaces whose address read failed. Do not
		// recurse through that same cache indefinitely: propagate the read error
		// so the monitor retains its last valid snapshot and retries later.
		iface.FlushCache()
		return readDefaultInterface(*netInterface)
	}
	return nil, err
}

func (f *defaultInterfaceFinder) ByIndex(index int) (*control.Interface, error) {
	ifaces, err := iface.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, netInterface := range ifaces {
		if netInterface.Index == index {
			return readDefaultInterface(control.Interface(*netInterface).NetInterface())
		}
	}
	netInterface, err := net.InterfaceByIndex(index)
	if err == nil {
		iface.FlushCache()
		return readDefaultInterface(*netInterface)
	}
	return nil, iface.ErrIfaceNotFound
}

// Read the monitored interface's complete address list. The shared interface
// cache cannot report a per-interface address error and historically accepted
// non-contiguous masks as /0. Neither is a valid source-loss snapshot.
func readDefaultInterface(netInterface net.Interface) (*control.Interface, error) {
	addresses, err := anet.InterfaceAddrsByInterface(&netInterface)
	if err != nil {
		return nil, fmt.Errorf("read addresses for interface %s: %w", netInterface.Name, err)
	}
	prefixes, err := interfacePrefixesFromAddresses(addresses)
	if err != nil {
		return nil, fmt.Errorf("read addresses for interface %s: %w", netInterface.Name, err)
	}
	result := control.InterfaceFromNetAddrs(netInterface, prefixes)
	result.HardwareAddr = append(net.HardwareAddr(nil), result.HardwareAddr...)
	return &result, nil
}

func interfacePrefixesFromAddresses(addresses []net.Addr) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(addresses))
	for _, rawAddress := range addresses {
		var address netip.Addr
		var ones int
		switch rawAddress := rawAddress.(type) {
		case *net.IPNet:
			if rawAddress == nil {
				return nil, fmt.Errorf("nil interface address")
			}
			var valid bool
			address, valid = netip.AddrFromSlice(rawAddress.IP)
			var bits int
			ones, bits = rawAddress.Mask.Size()
			if !valid || bits != 32 && bits != 128 {
				return nil, fmt.Errorf("invalid interface address or mask: %v", rawAddress)
			}
			if bits == 32 {
				address = address.Unmap()
				if !address.Is4() {
					return nil, fmt.Errorf("IPv4 mask on IPv6 interface address: %v", rawAddress)
				}
			} else if !address.Is6() {
				return nil, fmt.Errorf("IPv6 mask on IPv4 interface address: %v", rawAddress)
			}
		case *net.IPAddr:
			if rawAddress == nil {
				return nil, fmt.Errorf("nil interface address")
			}
			address, _ = netip.AddrFromSlice(rawAddress.IP)
			address = address.Unmap()
			ones = address.BitLen()
		default:
			return nil, fmt.Errorf("unsupported interface address: %T", rawAddress)
		}
		prefixes = append(prefixes, netip.PrefixFrom(address, ones))
	}
	return canonicalInterfacePrefixes(prefixes)
}

// Preserve the source address, canonicalize mapped IPv4 and ordering, and
// reject incomplete snapshots instead of treating malformed entries as losses.
// IPv6 network prefixes are masked only when comparing source connectivity.
func canonicalInterfacePrefixes(prefixes []netip.Prefix) ([]netip.Prefix, error) {
	canonical := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		if !prefix.IsValid() {
			return nil, fmt.Errorf("invalid interface prefix: %v", prefix)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("invalid mapped IPv4 interface mask: %v", prefix)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		canonical = append(canonical, prefix)
	}
	sort.Slice(canonical, func(i, j int) bool {
		if order := canonical[i].Addr().Compare(canonical[j].Addr()); order != 0 {
			return order < 0
		}
		return canonical[i].Bits() < canonical[j].Bits()
	})
	unique := canonical[:0]
	for _, prefix := range canonical {
		if len(unique) == 0 || unique[len(unique)-1] != prefix {
			unique = append(unique, prefix)
		}
	}
	return unique, nil
}

func (f *defaultInterfaceFinder) ByAddr(addr netip.Addr) (*control.Interface, error) {
	netInterface, err := iface.ResolveInterfaceByAddr(addr)
	if err != nil {
		return nil, err
	}
	return (*control.Interface)(netInterface), nil
}
