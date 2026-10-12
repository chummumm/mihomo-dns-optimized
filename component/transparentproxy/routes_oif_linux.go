//go:build linux

package transparentproxy

import "golang.org/x/sys/unix"

// Native sing-tun installs an IPv6 oif=TUN lookup even with auto-route=false.
// Linux matches this selector against the flow's explicitly requested output
// device; ordinary input and unbound, global-destination output use oif=0.
// A wildcard local capture can still include a socket bound to the TUN, so the
// selector alone is insufficient: every possible result of its table must stay
// on that same real L3 TUN or terminate, never escape through another device.
// This is conflict assessment only. The native rule/table never becomes owned.
func (s *linuxRouteSystem) safeOutputBoundTUNRule(observation routeObservation, rule observedRule, policies []routePolicy) bool {
	if rule.Family != unix.AF_INET6 || !rule.OIFOnly || rule.Action != unix.FR_ACT_TO_TBL || observation.Links == nil {
		return false
	}
	if rule.Table == 0 || rule.Table >= 253 && rule.Table <= 255 {
		return false
	}
	for _, group := range [][]routePolicy{policies, s.state.Policies} {
		for _, policy := range group {
			if rule.Family == routeFamily(policy.IPv6) && rule.Table == policy.Table {
				return false
			}
		}
	}
	index, present := observation.Links.Interfaces[rule.OIFName]
	if !present {
		// After an unexpected exit Linux detaches the native rule and removes
		// routes through the vanished TUN. An empty table cannot divert any
		// flow, and must not prevent the replacement TUN from being created.
		if !rule.OIFDetached {
			return false
		}
		for _, route := range observation.Routes {
			if route.Family == rule.Family && route.Table == rule.Table {
				return false
			}
		}
		return true
	}
	if rule.OIFDetached || !observation.Links.TUN[index] {
		return false
	}
	for _, route := range observation.Routes {
		if route.Family != rule.Family || route.Table != rule.Table {
			continue
		}
		// rt6_fill_node includes next-hop health in rtm_flags. DEAD/LINKDOWN
		// cannot redirect the route elsewhere, and must not strand the close
		// fence while a real TUN is down but has not yet been removed.
		if !route.Plain || route.Flags&^uint32(unix.RTNH_F_DEAD|unix.RTNH_F_LINKDOWN) != 0 {
			return false
		}
		switch route.Type {
		case unix.RTN_UNICAST:
			if route.IfIndex != index {
				return false
			}
		case unix.RTN_BLACKHOLE, unix.RTN_UNREACHABLE, unix.RTN_PROHIBIT:
			// These terminate a route lookup. RTN_THROW deliberately is not
			// included: it transfers lookup control instead of dropping.
		default:
			return false
		}
	}
	return true
}
