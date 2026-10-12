//go:build linux

package transparentproxy

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Native sing-tun installs an IPv6 oif=TUN lookup even with auto-route=false.
// Linux matches this selector against the flow's explicitly requested output
// device; ordinary input and unbound, global-destination output use oif=0.
// A wildcard local capture can still include a socket bound to the TUN, so the
// selector alone is insufficient: every possible result of its table must stay
// on that same real L3 TUN or terminate, never escape through another device.
// This is conflict assessment only. The native rule/table never becomes owned.
func (s *linuxRouteSystem) outputBoundTUNRuleConflict(observation routeObservation, rule observedRule, policies []routePolicy) string {
	if rule.Family != unix.AF_INET6 || !rule.OIFOnly || rule.Action != unix.FR_ACT_TO_TBL || observation.Links == nil {
		return fmt.Sprintf("not a verified IPv6 output-only lookup (action %d, mark-set %t, mask %#x, flags %#x, selector attribute %d)", rule.Action, rule.MarkSet, rule.Mask, rule.Flags, rule.UnsupportedAttribute)
	}
	if rule.Table == 0 || rule.Table >= 253 && rule.Table <= 255 {
		return "output lookup uses a reserved table"
	}
	for _, group := range [][]routePolicy{policies, s.state.Policies} {
		for _, policy := range group {
			if rule.Family == routeFamily(policy.IPv6) && rule.Table == policy.Table {
				return "output lookup uses a managed table"
			}
		}
	}
	index, present := observation.Links.Interfaces[rule.OIFName]
	if !present {
		// After an unexpected exit Linux detaches the native rule and removes
		// routes through the vanished TUN. An empty table cannot divert any
		// flow, and must not prevent the replacement TUN from being created.
		if !rule.OIFDetached {
			return "output device is absent but the rule is not detached"
		}
		for _, route := range observation.Routes {
			if route.Family == rule.Family && route.Table == rule.Table {
				return "detached output device still has routes in its table"
			}
		}
		return ""
	}
	if rule.OIFDetached || !observation.Links.TUN[index] {
		return fmt.Sprintf("output device index %d is detached or is not an L3 TUN", index)
	}
	for _, route := range observation.Routes {
		if route.Family != rule.Family || route.Table != rule.Table {
			continue
		}
		// rt6_fill_node includes next-hop health in rtm_flags. DEAD/LINKDOWN
		// cannot redirect the route elsewhere, and must not strand the close
		// fence while a real TUN is down but has not yet been removed.
		if !route.SinglePath || route.Flags&^uint32(unix.RTNH_F_DEAD|unix.RTNH_F_LINKDOWN) != 0 {
			return fmt.Sprintf("unsupported target route (type %d, dev %d, gateway %v, flags %#x, attribute %d)", route.Type, route.IfIndex, route.Gateway, route.Flags, route.UnsupportedAttribute)
		}
		switch route.Type {
		case unix.RTN_UNICAST:
			if route.IfIndex != index {
				return fmt.Sprintf("target route exits dev %d instead of TUN %d (gateway %v)", route.IfIndex, index, route.Gateway)
			}
			// Native sing-tun may use the next IPv6 address as its gateway.
			// Linux fixes fib_nh_dev when installing this single next hop;
			// rt6_nexthop supplies only the neighbour address on that device.
			// No gateway recursion can select a WAN at packet output. LWT,
			// multipath and nexthop objects remain excluded by SinglePath.
			if gateway := route.Gateway; gateway.IsValid() && (!gateway.Is6() || gateway.Is4In6() || gateway.IsUnspecified() || gateway.IsMulticast()) {
				return fmt.Sprintf("target route has an invalid IPv6 unicast gateway %v", gateway)
			}
		case unix.RTN_BLACKHOLE, unix.RTN_UNREACHABLE, unix.RTN_PROHIBIT:
			// These terminate a route lookup. RTN_THROW deliberately is not
			// included: it transfers lookup control instead of dropping.
			if !route.Plain {
				return "terminating target route has a gateway"
			}
		default:
			return fmt.Sprintf("target route type %d does not stay on the TUN or terminate", route.Type)
		}
	}
	return ""
}
