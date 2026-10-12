package sing_tun

import (
	"bytes"
	"net"
	"net/netip"
	"sync"

	"github.com/metacubex/mihomo/component/iface"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/sing/common/control"
)

// defaultInterfaceChangeHandler separates refreshed interface information from
// loss of an exit or source prefix. RA renewal, address ordering and IPv6 privacy
// rotation do not invalidate every DNS connection. A removed IPv4 source or
// public IPv6 prefix, a changed device, and loss/recovery of the exit reset them.
//
// The monitor has no view of individual DNS sockets. A retired temporary or ULA
// source may still be in use even while the tracked prefix/family remains; that
// transport uses its normal I/O failure and reconnect path rather than clearing
// all DNS pools.
// Interface address enumeration also omits IPv6 DAD state and address lifetimes:
// source/prefix presence here does not establish reachability or full validity.
type defaultInterfaceChangeHandler struct {
	access      sync.Mutex
	closed      bool
	initialized bool
	previous    *control.Interface

	flushCache       func()
	resetConnections func()
}

func newDefaultInterfaceChangeHandler() *defaultInterfaceChangeHandler {
	return &defaultInterfaceChangeHandler{
		flushCache:       iface.FlushCache,
		resetConnections: resolver.ResetConnection,
	}
}

func (h *defaultInterfaceChangeHandler) Update(current *control.Interface, event int) {
	h.access.Lock()
	defer h.access.Unlock()
	if h.closed {
		return
	}

	// Benign changes must still refresh source selection for future dials.
	h.flushCache()
	// An incomplete address read is not evidence that a source disappeared.
	// Keep our last valid snapshot; the finder also returns errors to the monitor
	// so it retries instead of publishing an incomplete interface.
	if current != nil {
		addresses, err := canonicalInterfacePrefixes(current.Addresses)
		if err != nil {
			log.Debugln("[TUN] ignore incomplete default interface update: %s", err)
			return
		}
		snapshot := *current
		snapshot.HardwareAddr = append(net.HardwareAddr(nil), current.HardwareAddr...)
		snapshot.Addresses = addresses
		current = &snapshot
	}
	reason := defaultInterfaceResetReason(h.previous, current, event)
	if !h.initialized {
		reason = "initial interface discovery"
	}
	h.previous = current
	h.initialized = true
	if reason == "" {
		return
	}
	if current != nil {
		log.Warnln("[TUN] default interface changed by monitor (%s), => %s", reason, current.Name)
	} else {
		log.Errorln("[TUN] default interface lost by monitor")
	}
	h.resetConnections()
}

// Close is a barrier for callback side effects, including callbacks copied by
// the upstream monitor before it unregisters from network events. The resolver
// API retains its existing asynchronous reset semantics.
func (h *defaultInterfaceChangeHandler) Close() {
	if h == nil {
		return
	}
	h.access.Lock()
	defer h.access.Unlock()
	h.closed = true
	h.previous = nil
}

func defaultInterfaceResetReason(previous, current *control.Interface, event int) string {
	if event != 0 {
		return "network policy changed" // e.g. Android VPN policy, without a new device
	}
	if previous == nil {
		if current != nil {
			return "default interface available"
		}
		return ""
	}
	if current == nil {
		return "default interface lost"
	}
	if previous.Index != current.Index || previous.Name != current.Name || !bytes.Equal(previous.HardwareAddr, current.HardwareAddr) {
		return "interface identity changed"
	}
	// Broadcast/multicast capabilities are unrelated to established DNS streams.
	// A carrier transition, link type change or smaller/new MTU can invalidate
	// the path, including a cached QUIC path MTU, without changing its addresses.
	const pathFlags = net.FlagUp | net.FlagRunning | net.FlagPointToPoint
	if previous.Flags&pathFlags != current.Flags&pathFlags || previous.MTU != current.MTU {
		return "interface path changed"
	}
	oldSources := interfaceSources(previous.Addresses)
	newSources := interfaceSources(current.Addresses)
	for address := range oldSources.ipv4 {
		if _, exists := newSources.ipv4[address]; !exists {
			return "IPv4 source address removed"
		}
	}
	for prefix := range oldSources.publicIPv6 {
		if _, exists := newSources.publicIPv6[prefix]; !exists {
			return "public IPv6 source prefix removed"
		}
	}
	if oldSources.nonLinkLocalIPv6 && !newSources.nonLinkLocalIPv6 {
		return "IPv6 source family lost"
	}
	return ""
}

type interfaceSourceState struct {
	ipv4             map[netip.Addr]struct{}
	publicIPv6       map[netip.Prefix]struct{}
	nonLinkLocalIPv6 bool
}

// IPv4 sources are retained exactly; IPv6 interface IDs may rotate within a
// public prefix. ULA prefix/address churn alone does not clear all DNS pools.
// Still detect loss of the last enumerated non-link-local IPv6 source, including
// ULA-only sources. Keeping a ULA or link-local address cannot hide a public-prefix
// loss. Additions do not invalidate existing transports, even a new IP family.
func interfaceSources(addresses []netip.Prefix) interfaceSourceState {
	sources := interfaceSourceState{
		ipv4:       make(map[netip.Addr]struct{}),
		publicIPv6: make(map[netip.Prefix]struct{}),
	}
	for _, prefix := range addresses {
		address := prefix.Addr()
		if !prefix.IsValid() || (!address.IsGlobalUnicast() && !address.IsLinkLocalUnicast()) {
			continue
		}
		if address.Is4In6() {
			address = address.Unmap()
		}
		if address.Is4() {
			sources.ipv4[address] = struct{}{}
		} else if address.IsGlobalUnicast() {
			sources.nonLinkLocalIPv6 = true
			if !address.IsPrivate() {
				sources.publicIPv6[prefix.Masked()] = struct{}{}
			}
		}
	}
	return sources
}
