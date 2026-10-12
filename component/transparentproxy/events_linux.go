//go:build linux

package transparentproxy

import (
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"
)

const notificationCacheLimit = 8192

// This cache describes consecutive kernel notifications, not authoritative
// network state. A new subscription starts empty; packet loss destroys it.
// Address comparison uses Snapshot's usable prefix identity. Link comparison
// retains carrier, operational state and all other routing-related attributes:
// the kernel's route lookup depends on more than the administrative UP flag.
// Removals still invalidate a complete snapshot rather than applying a delta.
type systemNotificationFilter struct {
	links     map[int]string
	addresses map[networkAddressIdentity]bool
	lastRA4   string
	lastRA6   string
}

type networkAddressIdentity struct {
	index  int
	prefix netip.Prefix
}

func (f *systemNotificationFilter) notification(packet wirePacket, nftables bool) (changed, urgent bool, err error) {
	changed, err = validateSystemNotification(packet, nftables)
	if err != nil || !changed {
		return changed, false, err
	}
	if nftables {
		// NEWGEN is a committed transaction. Without reading its contents we
		// cannot exclude a flush/removal of our capture rules, including when
		// other software shares the namespace. Never debounce this signal.
		return true, true, nil
	}
	switch packet.Type {
	case unix.RTM_NEWLINK, unix.RTM_DELLINK:
		link, _ := decodeNetworkLink(packet.Data)
		if packet.Type == unix.RTM_DELLINK {
			delete(f.links, link.Index)
			for address := range f.addresses {
				if address.index == link.Index {
					delete(f.addresses, address)
				}
			}
			f.lastRA4, f.lastRA6 = "", ""
			return true, true, nil
		}
		attributes, _ := parseWireAttributes(packet.Data[16:])
		// ifi_change is a notification mask, and stats are counters. Keep
		// every other attribute, including future ones, rather than guessing
		// which carrier/VRF/device change can influence a policy lookup.
		identityBytes := append([]byte(nil), packet.Data[:12]...)
		for _, attribute := range attributes {
			if kind := attribute.Type & wireAttributeTypeMask; kind != unix.IFLA_STATS && kind != unix.IFLA_STATS64 {
				identityBytes = appendWireAttribute(identityBytes, attribute.Type, attribute.Data)
			}
		}
		identity := string(identityBytes)
		previous, known := f.links[link.Index]
		if known && previous == identity {
			return false, false, nil
		}
		if f.links == nil || len(f.links) >= notificationCacheLimit {
			f.links = make(map[int]string)
		}
		f.links[link.Index] = identity
		f.lastRA4, f.lastRA6 = "", ""
		return true, true, nil
	case unix.RTM_NEWADDR, unix.RTM_DELADDR:
		index, prefix, usable, _ := decodeNetworkAddress(packet.Data)
		if !prefix.IsValid() {
			return false, false, nil
		}
		identity := networkAddressIdentity{index, prefix}
		if packet.Type == unix.RTM_DELADDR {
			delete(f.addresses, identity)
			return true, true, nil
		}
		previous, known := f.addresses[identity]
		if known && previous == usable {
			// Positive valid-lifetime refreshes and preferred-lifetime changes
			// do not change a usable prefix. Expiry and DAD failure still do.
			return false, false, nil
		}
		if f.addresses == nil || len(f.addresses) >= notificationCacheLimit {
			f.addresses = make(map[networkAddressIdentity]bool)
		}
		f.addresses[identity] = usable
		return true, !usable, nil
	case unix.RTM_NEWROUTE, unix.RTM_DELROUTE:
		return f.routeNotification(packet)
	case unix.RTM_NEWRULE, unix.RTM_DELRULE:
		// Rule selectors can change the result of any policy lookup. Even
		// an apparently unrelated rule may precede our guard priorities.
		f.lastRA4, f.lastRA6 = "", ""
		return true, true, nil
	case unix.RTM_NEWNETCONF, unix.RTM_DELNETCONF:
		return true, true, nil
	}
	return true, false, nil
}

func defaultRouteProbe(ipv6 bool) netip.Addr {
	if ipv6 {
		return netip.MustParseAddr("2606:4700:4700::1111")
	}
	return netip.MustParseAddr("1.1.1.1")
}

func (f *systemNotificationFilter) routeNotification(packet wirePacket) (bool, bool, error) {
	route, err := decodeObservedRoute(packet.Data)
	if err != nil {
		return false, false, err
	}
	if route.Family != unix.AF_INET && route.Family != unix.AF_INET6 {
		return false, false, nil
	}
	lastRA := &f.lastRA4
	if route.Family == unix.AF_INET6 {
		lastRA = &f.lastRA6
	}
	// Main is reserved and can never be one of our managed or native TUN
	// tables. Routes outside the probe's destination prefix do not affect
	// Snapshot's chosen exit. This includes ordinary RA RIO/Thread ULA routes,
	// but deliberately includes non-default routes that cover either probe.
	if route.Table == unix.RT_TABLE_MAIN && route.Protocol != linuxOwnerProtocol {
		attributes, err := parseWireAttributes(packet.Data[12:])
		if err != nil {
			return false, false, err
		}
		probe := defaultRouteProbe(route.Family == unix.AF_INET6)
		destination := netip.IPv4Unspecified()
		if route.Family == unix.AF_INET6 {
			destination = netip.IPv6Unspecified()
		}
		hasDestination := packet.Data[1] == 0
		for _, attribute := range attributes {
			if attribute.Type&wireAttributeTypeMask == unix.RTA_DST {
				destination, _ = netip.AddrFromSlice(attribute.Data)
				hasDestination = true
			}
		}
		prefix := netip.PrefixFrom(destination, int(packet.Data[1]))
		if !hasDestination || !prefix.IsValid() {
			return false, false, fmt.Errorf("%w: missing or invalid route prefix", errIncompleteNetlink)
		}
		if !prefix.Contains(probe) {
			return false, false, nil
		}
		if packet.Type == unix.RTM_NEWROUTE && route.Protocol == unix.RTPROT_RA {
			identity, renewable, err := renewableRouteIdentity(packet.Data, attributes)
			if err != nil {
				return false, false, err
			}
			if renewable {
				unchanged := *lastRA == identity
				*lastRA = identity
				return !unchanged, false, nil
			}
		}
		*lastRA = ""
		return true, packet.Type == unix.RTM_DELROUTE, nil
	}
	*lastRA = ""
	// Other tables may serve our guards or an output-bound native TUN rule.
	// Replacements are as urgent as deletions: they can redirect captured data.
	return true, true, nil
}

// Compare only consecutive RA updates of the same effective route, so a
// gateway/preference/metric change and a later return to the old route are
// both observed. Other relevant routing changes clear the previous identity.
func renewableRouteIdentity(data []byte, attributes []wireAttribute) (string, bool, error) {
	identity := append([]byte(nil), data[:12]...)
	renewable := false
	for _, attribute := range attributes {
		switch attribute.Type & wireAttributeTypeMask {
		case unix.RTA_EXPIRES:
			if len(attribute.Data) != 4 {
				return "", false, fmt.Errorf("%w: invalid route lifetime", errIncompleteNetlink)
			}
			if wireOrder.Uint32(attribute.Data) == 0 {
				return "", false, nil
			}
			renewable = true
		case unix.RTA_CACHEINFO:
			if len(attribute.Data) != 32 {
				return "", false, fmt.Errorf("%w: invalid route cache information", errIncompleteNetlink)
			}
			// IPv6 rt6_fill_node reports expiry in this signed countdown,
			// usually without a separate RTA_EXPIRES attribute.
			if int32(wireOrder.Uint32(attribute.Data[8:12])) <= 0 {
				return "", false, nil
			}
			renewable = true
			// Only cache error is semantic. Usage counters and timestamps,
			// including the countdown, do not identify a routing change.
			identity = appendWireAttribute(identity, attribute.Type, attribute.Data[12:16])
		default:
			identity = appendWireAttribute(identity, attribute.Type, attribute.Data)
		}
	}
	return string(identity), renewable, nil
}
