//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

const (
	activeRouteMetric    uint32 = 100
	blackholeRouteMetric uint32 = 1 << 30
)

type ownedRoute struct {
	Backend Mode
	Family  uint8
	Table   uint32
	Metric  uint32
	Type    uint8
	Scope   uint8
	IfIndex int
}

type ownedRule struct {
	Backend  Mode
	Family   uint8
	Mark     uint32
	Table    uint32
	Priority uint32
	Action   uint8
}

type observedRoute struct {
	ownedRoute
	Protocol uint8
	Default  bool
	Plain    bool
	Flags    uint32
	// A single gateway does not make a route owned. Record its otherwise
	// ordinary shape separately for the native output-bound TUN lookup.
	Gateway              netip.Addr
	SinglePath           bool
	UnsupportedAttribute uint16
}

type observedRule struct {
	ownedRule
	Protocol             uint8
	MarkSet              bool
	Mask                 uint32
	Plain                bool
	Flags                uint32
	UnsupportedAttribute uint16
	// An output-device selector is never part of our owned rule identity.
	// Keep its exact shape separately to assess the native IPv6 TUN helper.
	OIFName     string
	OIFOnly     bool
	OIFDetached bool
}

type routeObservation struct {
	Routes  []observedRoute
	Rules   []observedRule
	Links   *routeLinkState
	changed bool
}

func routeFamily(ipv6 bool) uint8 {
	if ipv6 {
		return unix.AF_INET6
	}
	return unix.AF_INET
}

func sameRoute(a, b ownedRoute) bool { a.Backend, b.Backend = "", ""; return a == b }
func sameRule(a, b ownedRule) bool   { a.Backend, b.Backend = "", ""; return a == b }
func samePolicy(a, b routePolicy) bool {
	a.IfIndex, b.IfIndex, a.Ready, b.Ready = 0, 0, false, false
	return a == b
}

func blackholeRoute(policy routePolicy) ownedRoute {
	return ownedRoute{Backend: policy.Backend, Family: routeFamily(policy.IPv6), Table: policy.Table, Metric: blackholeRouteMetric, Type: unix.RTN_BLACKHOLE, Scope: unix.RT_SCOPE_UNIVERSE}
}

func policyRules(policy routePolicy) (guard, lookup ownedRule) {
	lookup = ownedRule{Backend: policy.Backend, Family: routeFamily(policy.IPv6), Mark: policy.Mark, Table: policy.Table, Priority: policy.Priority, Action: unix.FR_ACT_TO_TBL}
	guard = lookup
	guard.Table, guard.Priority, guard.Action = 0, policy.Priority+1, unix.FR_ACT_BLACKHOLE
	return
}

func activeRoute(policy routePolicy, loopback int) ownedRoute {
	route := ownedRoute{Backend: policy.Backend, Family: routeFamily(policy.IPv6), Table: policy.Table, Metric: activeRouteMetric, Type: unix.RTN_UNICAST, Scope: unix.RT_SCOPE_LINK, IfIndex: policy.IfIndex}
	if policy.Backend == ModeTProxy {
		route.Type, route.Scope, route.IfIndex = unix.RTN_LOCAL, unix.RT_SCOPE_HOST, loopback
	}
	// IPv6 rt6_fill_node always reports RT_SCOPE_UNIVERSE, including local
	// and device routes. Use that canonical value for both requests and identity.
	if policy.IPv6 {
		route.Scope = unix.RT_SCOPE_UNIVERSE
	}
	return route
}

func validateRoutePolicy(policy routePolicy) error {
	if policy.Backend != ModeTProxy && policy.Backend != ModeTUN {
		return errors.New("invalid managed route backend")
	}
	if policy.Mark == 0 || policy.Mark == DefaultOutboundMark || policy.Table == 0 || policy.Table >= 253 && policy.Table <= 255 || policy.Priority == 0 || policy.Priority > 32764 {
		return errors.New("invalid managed route mark, table or priority pair")
	}
	if policy.Ready && policy.Backend == ModeTUN && policy.IfIndex <= 0 {
		return errors.New("ready TUN route requires its actual interface index")
	}
	return nil
}

func validateRouteState(state routeState) error {
	if len(state.Policies) > 128 || len(state.Routes) > 256 || len(state.Rules) > 256 || len(state.Sysctls) > 4096 {
		return errors.New("transparent proxy ownership exceeds limits")
	}
	for _, policy := range state.Policies {
		if err := validateRoutePolicy(policy); err != nil {
			return err
		}
	}
	for _, route := range state.Routes {
		valid := false
		for _, policy := range state.Policies {
			if route.Backend != policy.Backend || route.Family != routeFamily(policy.IPv6) || route.Table != policy.Table {
				continue
			}
			if sameRoute(route, blackholeRoute(policy)) {
				valid = true
				break
			}
			if route.Metric != activeRouteMetric || route.IfIndex <= 0 {
				continue
			}
			if route.Scope == activeRoute(policy, route.IfIndex).Scope && (policy.Backend == ModeTUN && route.Type == unix.RTN_UNICAST || policy.Backend == ModeTProxy && route.Type == unix.RTN_LOCAL) {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("ownership contains an invalid managed route")
		}
	}
	for _, rule := range state.Rules {
		valid := false
		for _, policy := range state.Policies {
			guard, lookup := policyRules(policy)
			if sameRule(rule, guard) || sameRule(rule, lookup) {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("ownership contains an invalid managed rule")
		}
	}
	for _, setting := range state.Sysctls {
		if err := validateOwnedSysctl(setting); err != nil {
			return err
		}
	}
	return nil
}

func (s *linuxRouteSystem) routeSnapshot(ctx context.Context) (routeObservation, error) {
	var observation routeObservation
	messages, err := s.request.Execute(ctx, unix.RTM_GETROUTE, unix.NLM_F_DUMP, make([]byte, 12))
	if err != nil {
		return observation, fmt.Errorf("read all routing tables: %w", err)
	}
	for _, message := range messages {
		if message.Type != unix.RTM_NEWROUTE {
			return observation, fmt.Errorf("%w: unexpected route dump type", errIncompleteNetlink)
		}
		if len(message.Data) < 12 {
			return observation, fmt.Errorf("%w: short route", errIncompleteNetlink)
		}
		if message.Data[0] != unix.AF_INET && message.Data[0] != unix.AF_INET6 {
			continue
		}
		if wireOrder.Uint32(message.Data[8:12])&unix.RTM_F_CLONED != 0 {
			// Route-cache exceptions are observations derived from a FIB entry,
			// not independently owned routing-table objects.
			continue
		}
		route, err := decodeObservedRoute(message.Data)
		if err != nil {
			return observation, err
		}
		observation.Routes = append(observation.Routes, route)
	}
	messages, err = s.request.Execute(ctx, unix.RTM_GETRULE, unix.NLM_F_DUMP, make([]byte, 12))
	if err != nil {
		return observation, fmt.Errorf("read policy rules: %w", err)
	}
	for _, message := range messages {
		if message.Type != unix.RTM_NEWRULE {
			return observation, fmt.Errorf("%w: unexpected rule dump type", errIncompleteNetlink)
		}
		if len(message.Data) < 12 {
			return observation, fmt.Errorf("%w: short rule", errIncompleteNetlink)
		}
		if message.Data[0] != unix.AF_INET && message.Data[0] != unix.AF_INET6 {
			continue
		}
		rule, err := decodeObservedRule(message.Data)
		if err != nil {
			return observation, err
		}
		observation.Rules = append(observation.Rules, rule)
	}
	if slices.ContainsFunc(observation.Rules, func(rule observedRule) bool { return rule.OIFOnly }) {
		links, err := s.readRouteLinks(ctx)
		if err != nil {
			return observation, err
		}
		observation.Links = &links
	}
	return observation, nil
}

func decodeObservedRoute(data []byte) (observedRoute, error) {
	var route observedRoute
	if len(data) < 12 {
		return route, fmt.Errorf("%w: short route", errIncompleteNetlink)
	}
	route.Family, route.Table, route.Protocol, route.Scope, route.Type = data[0], uint32(data[4]), data[5], data[6], data[7]
	route.Default, route.Plain = data[1] == 0, data[2] == 0 && data[3] == 0
	route.SinglePath = route.Plain
	route.Flags = wireOrder.Uint32(data[8:12])
	attributes, err := parseWireAttributes(data[12:])
	if err != nil {
		return route, err
	}
	for _, attribute := range attributes {
		switch attribute.Type & wireAttributeTypeMask {
		case unix.RTA_TABLE, unix.RTA_PRIORITY, unix.RTA_OIF:
			if len(attribute.Data) != 4 {
				return route, fmt.Errorf("%w: invalid route integer", errIncompleteNetlink)
			}
			value := wireOrder.Uint32(attribute.Data)
			switch attribute.Type & wireAttributeTypeMask {
			case unix.RTA_TABLE:
				route.Table = value
			case unix.RTA_PRIORITY:
				route.Metric = value
			case unix.RTA_OIF:
				route.IfIndex = int(value)
			}
		case unix.RTA_DST:
			length := 4
			if route.Family == unix.AF_INET6 {
				length = 16
			}
			if len(attribute.Data) != length {
				return route, fmt.Errorf("%w: invalid route destination", errIncompleteNetlink)
			}
			for _, value := range attribute.Data {
				if value != 0 {
					route.Default = false
				}
			}
		case unix.RTA_GATEWAY:
			length := 4
			if route.Family == unix.AF_INET6 {
				length = 16
			}
			if len(attribute.Data) != length || route.Gateway.IsValid() {
				return route, fmt.Errorf("%w: invalid or duplicate route gateway", errIncompleteNetlink)
			}
			route.Gateway, _ = netip.AddrFromSlice(attribute.Data)
			route.Plain = false
		case unix.RTA_PREF:
			// IPv6 reports the kernel's default router preference even when the
			// creator did not specify it. A non-default preference is external.
			if len(attribute.Data) != 1 {
				return route, fmt.Errorf("%w: invalid route preference", errIncompleteNetlink)
			}
			if attribute.Data[0] != 0 {
				route.Plain = false
				route.SinglePath = false
				route.UnsupportedAttribute = unix.RTA_PREF
			}
		case unix.RTA_CACHEINFO:
			// Usage counters and cache timestamps are kernel observations.
		default:
			route.Plain = false
			route.SinglePath = false
			if route.UnsupportedAttribute == 0 {
				route.UnsupportedAttribute = attribute.Type & wireAttributeTypeMask
			}
		}
	}
	// IPv6 reject routes are internally attached to the namespace loopback by
	// fib6_nh_init. Their supplied next hop is ignored and they remain independent
	// of TUN deletion; represent this implicit device just as the request did.
	if route.Family == unix.AF_INET6 && route.Type == unix.RTN_BLACKHOLE {
		route.IfIndex = 0
	}
	return route, nil
}

func decodeObservedRule(data []byte) (observedRule, error) {
	var rule observedRule
	if len(data) < 12 {
		return rule, fmt.Errorf("%w: short rule", errIncompleteNetlink)
	}
	rule.Family, rule.Table, rule.Action = data[0], uint32(data[4]), data[7]
	flags := wireOrder.Uint32(data[8:12])
	rule.Flags = flags
	plainSelector := data[1] == 0 && data[2] == 0 && data[3] == 0
	rule.Plain = plainSelector && flags == 0
	oifOnly := plainSelector && flags&^uint32(unix.FIB_RULE_OIF_DETACHED) == 0
	rule.OIFDetached = flags&unix.FIB_RULE_OIF_DETACHED != 0
	rule.Mask = ^uint32(0)
	attributes, err := parseWireAttributes(data[12:])
	if err != nil {
		return rule, err
	}
	for _, attribute := range attributes {
		switch attribute.Type & wireAttributeTypeMask {
		case unix.FRA_TABLE, unix.FRA_PRIORITY, unix.FRA_FWMARK, unix.FRA_FWMASK, unix.FRA_SUPPRESS_PREFIXLEN, unix.FRA_SUPPRESS_IFGROUP:
			if len(attribute.Data) != 4 {
				return rule, fmt.Errorf("%w: invalid rule integer", errIncompleteNetlink)
			}
			value := wireOrder.Uint32(attribute.Data)
			switch attribute.Type & wireAttributeTypeMask {
			case unix.FRA_TABLE:
				rule.Table = value
			case unix.FRA_PRIORITY:
				rule.Priority = value
			case unix.FRA_FWMARK:
				rule.Mark, rule.MarkSet = value, true
				oifOnly = false
			case unix.FRA_FWMASK:
				rule.Mask = value
				oifOnly = false
			default:
				if value != ^uint32(0) {
					rule.Plain = false
					oifOnly = false
					rule.UnsupportedAttribute = attribute.Type & wireAttributeTypeMask
				}
			}
		case unix.FRA_OIFNAME:
			if rule.OIFName != "" || len(attribute.Data) < 2 || len(attribute.Data) > unix.IFNAMSIZ || attribute.Data[len(attribute.Data)-1] != 0 {
				return rule, fmt.Errorf("%w: invalid rule output interface", errIncompleteNetlink)
			}
			rule.OIFName = string(attribute.Data[:len(attribute.Data)-1])
			if strings.IndexByte(rule.OIFName, 0) >= 0 {
				return rule, fmt.Errorf("%w: embedded null in rule output interface", errIncompleteNetlink)
			}
			rule.Plain = false
		case unix.FRA_PROTOCOL:
			if len(attribute.Data) != 1 {
				return rule, fmt.Errorf("%w: invalid rule protocol", errIncompleteNetlink)
			}
			rule.Protocol = attribute.Data[0]
		case unix.FRA_PAD:
		default:
			rule.Plain = false
			oifOnly = false
			if rule.UnsupportedAttribute == 0 {
				rule.UnsupportedAttribute = attribute.Type & wireAttributeTypeMask
			}
		}
	}
	rule.OIFOnly = oifOnly && rule.OIFName != ""
	return rule, nil
}

func (s *linuxRouteSystem) ownsRoute(route observedRoute) bool {
	return route.Protocol == linuxOwnerProtocol && route.Default && route.Plain && slices.ContainsFunc(s.state.Routes, func(owned ownedRoute) bool { return sameRoute(route.ownedRoute, owned) })
}

func (s *linuxRouteSystem) ownsRule(rule observedRule) bool {
	return rule.Protocol == linuxOwnerProtocol && rule.MarkSet && rule.Mask == ^uint32(0) && rule.Plain && slices.ContainsFunc(s.state.Rules, func(owned ownedRule) bool { return sameRule(rule.ownedRule, owned) })
}

func (s *linuxRouteSystem) checkRouteConflicts(observation routeObservation, policies []routePolicy) error {
	for _, policy := range policies {
		if err := validateRoutePolicy(policy); err != nil {
			return err
		}
		family := routeFamily(policy.IPv6)
		for _, route := range observation.Routes {
			if route.Family == family && route.Table == policy.Table && !s.ownsRoute(route) {
				return fmt.Errorf("transparent proxy table %d contains an external route (family %d)", policy.Table, family)
			}
		}
		for _, rule := range observation.Rules {
			if rule.Family != family || s.ownsRule(rule) {
				continue
			}
			if rule.Priority == 0 && rule.Table == unix.RT_TABLE_LOCAL && rule.Action == unix.FR_ACT_TO_TBL && !rule.MarkSet && rule.Plain {
				continue
			}
			priorityCollision := rule.Priority == policy.Priority || rule.Priority == policy.Priority+1
			// An inverted or otherwise extended selector cannot safely be ruled
			// out by comparing marks alone. Refuse ambiguous earlier policy.
			markMatches := !rule.Plain || !rule.MarkSet || policy.Mark&rule.Mask == rule.Mark&rule.Mask
			if priorityCollision || rule.Priority < policy.Priority && markMatches {
				reason := "priority is reserved by the managed lookup/guard pair"
				if !priorityCollision {
					reason = s.outputBoundTUNRuleConflict(observation, rule, policies)
					if reason == "" {
						continue
					}
				}
				return fmt.Errorf("transparent proxy mark %#x or priority pair %d/%d conflicts with external rule %d (family %d, table %d, oif %q): %s", policy.Mark, policy.Priority, policy.Priority+1, rule.Priority, family, rule.Table, rule.OIFName, reason)
			}
		}
	}
	return nil
}

func (s *linuxRouteSystem) EnsureRoutes(ctx context.Context, policies []routePolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureRoutesLocked(ctx, policies)
}

func (s *linuxRouteSystem) ensureRoutesLocked(ctx context.Context, policies []routePolicy) error {
	observation, err := s.routeSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := s.checkRouteConflicts(observation, policies); err != nil {
		return err
	}
	ipv6Available, err := s.ipv6StackAvailable()
	if err != nil {
		return err
	}
	policies = slices.Clone(policies)
	for i := range policies {
		if policies[i].IPv6 && !ipv6Available {
			policies[i].Ready = false
		}
	}
	loopback := 0
	if slices.ContainsFunc(policies, func(p routePolicy) bool { return p.Ready }) {
		messages, err := s.request.Execute(ctx, unix.RTM_GETLINK, unix.NLM_F_DUMP, make([]byte, 16))
		if err != nil {
			return err
		}
		up := make(map[int]bool)
		for _, message := range messages {
			link, err := decodeNetworkLink(message.Data)
			if err != nil {
				return err
			}
			up[link.Index] = link.Up
			if link.Name == "lo" && link.Loopback && link.Up {
				loopback = link.Index
			}
		}
		for _, policy := range policies {
			if !policy.Ready {
				continue
			}
			if policy.Backend == ModeTProxy && loopback == 0 {
				return errors.New("TProxy loopback device is not ready")
			}
			if policy.Backend == ModeTUN && !up[policy.IfIndex] {
				return errors.New("TUN device changed before route installation")
			}
		}
	}
	if err := s.updateState(func(state *routeState) {
		for _, policy := range policies {
			index := slices.IndexFunc(state.Policies, func(old routePolicy) bool { return samePolicy(old, policy) })
			if index < 0 {
				state.Policies = append(state.Policies, policy)
			} else {
				state.Policies[index] = policy
			}
		}
	}); err != nil {
		return err
	}
	// The device-independent route and terminal guard precede every lookup or
	// unicast route. Removing a device or a single lookup rule cannot reach main.
	for _, policy := range policies {
		if policy.IPv6 && !ipv6Available {
			continue
		}
		if err := s.ensureRoute(ctx, &observation, blackholeRoute(policy)); err != nil {
			return err
		}
		guard, _ := policyRules(policy)
		if err := s.ensureRule(ctx, &observation, guard); err != nil {
			return err
		}
	}
	for _, policy := range policies {
		if policy.IPv6 && !ipv6Available {
			continue
		}
		_, lookup := policyRules(policy)
		if err := s.ensureRule(ctx, &observation, lookup); err != nil {
			return err
		}
		wanted := activeRoute(policy, loopback)
		for _, old := range slices.Clone(s.state.Routes) {
			if old.Family != wanted.Family || old.Table != wanted.Table || old.Type == unix.RTN_BLACKHOLE {
				continue
			}
			if policy.Ready && sameRoute(old, wanted) {
				continue
			}
			if err := s.removeRoute(ctx, &observation, old); err != nil {
				return err
			}
		}
		if policy.Ready {
			if err := s.ensureRoute(ctx, &observation, wanted); err != nil {
				return err
			}
		}
	}
	if observation.changed {
		actual, err := s.routeSnapshot(ctx)
		if err != nil {
			return err
		}
		if err := s.checkRouteConflicts(actual, policies); err != nil {
			return err
		}
		for _, policy := range policies {
			if policy.IPv6 && !ipv6Available {
				continue
			}
			if err := s.verifyPolicyRoutes(actual, policy, loopback); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *linuxRouteSystem) verifyPolicyRoutes(observation routeObservation, policy routePolicy, loopback int) error {
	blackhole := blackholeRoute(policy)
	guard, lookup := policyRules(policy)
	hasRoute := slices.ContainsFunc(observation.Routes, func(route observedRoute) bool { return s.ownsRoute(route) && sameRoute(route.ownedRoute, blackhole) })
	hasGuard := slices.ContainsFunc(observation.Rules, func(rule observedRule) bool { return s.ownsRule(rule) && sameRule(rule.ownedRule, guard) })
	hasLookup := slices.ContainsFunc(observation.Rules, func(rule observedRule) bool { return s.ownsRule(rule) && sameRule(rule.ownedRule, lookup) })
	if !hasRoute || !hasGuard || !hasLookup {
		return errors.New("managed route protection did not survive verification")
	}
	if policy.Ready {
		active := activeRoute(policy, loopback)
		if !slices.ContainsFunc(observation.Routes, func(route observedRoute) bool { return s.ownsRoute(route) && sameRoute(route.ownedRoute, active) }) {
			return errors.New("managed forwarding route did not survive verification")
		}
		return nil
	}
	for _, route := range observation.Routes {
		if route.Family == blackhole.Family && route.Table == blackhole.Table && route.Type != unix.RTN_BLACKHOLE {
			return errors.New("a forwarding route remains while its backend is unavailable")
		}
	}
	return nil
}

func (s *linuxRouteSystem) ensureRoute(ctx context.Context, observation *routeObservation, route ownedRoute) error {
	for _, existing := range observation.Routes {
		if sameRoute(existing.ownedRoute, route) {
			if !s.ownsRoute(existing) {
				return errors.New("refusing to adopt an external route")
			}
			return s.rememberRoute(route)
		}
	}
	if err := s.rememberRoute(route); err != nil {
		return err
	}
	if _, err := s.request.Execute(ctx, unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK, marshalOwnedRoute(route)); err != nil {
		return fmt.Errorf("install managed route in table %d: %w", route.Table, err)
	}
	observation.Routes = append(observation.Routes, observedRoute{ownedRoute: route, Protocol: linuxOwnerProtocol, Default: true, Plain: true})
	observation.changed = true
	return nil
}

func (s *linuxRouteSystem) ensureRule(ctx context.Context, observation *routeObservation, rule ownedRule) error {
	for _, existing := range observation.Rules {
		if sameRule(existing.ownedRule, rule) {
			if !s.ownsRule(existing) {
				return errors.New("refusing to adopt an external policy rule")
			}
			return s.rememberRule(rule)
		}
	}
	if err := s.rememberRule(rule); err != nil {
		return err
	}
	if _, err := s.request.Execute(ctx, unix.RTM_NEWRULE, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK, marshalOwnedRule(rule)); err != nil {
		return fmt.Errorf("install managed policy rule %d: %w", rule.Priority, err)
	}
	observation.Rules = append(observation.Rules, observedRule{ownedRule: rule, Protocol: linuxOwnerProtocol, MarkSet: true, Mask: ^uint32(0), Plain: true})
	observation.changed = true
	return nil
}

func (s *linuxRouteSystem) rememberRoute(route ownedRoute) error {
	return s.updateState(func(state *routeState) {
		index := slices.IndexFunc(state.Routes, func(old ownedRoute) bool { return sameRoute(old, route) })
		if index < 0 {
			state.Routes = append(state.Routes, route)
		} else {
			state.Routes[index] = route
		}
	})
}

func (s *linuxRouteSystem) rememberRule(rule ownedRule) error {
	return s.updateState(func(state *routeState) {
		index := slices.IndexFunc(state.Rules, func(old ownedRule) bool { return sameRule(old, rule) })
		if index < 0 {
			state.Rules = append(state.Rules, rule)
		} else {
			state.Rules[index] = rule
		}
	})
}

func marshalOwnedRoute(route ownedRoute) []byte {
	data := make([]byte, 12)
	data[0], data[5], data[6], data[7] = route.Family, linuxOwnerProtocol, route.Scope, route.Type
	if route.Table < 256 {
		data[4] = byte(route.Table)
	}
	data = appendWireAttribute(data, unix.RTA_TABLE, wireUint32(route.Table))
	data = appendWireAttribute(data, unix.RTA_PRIORITY, wireUint32(route.Metric))
	if route.IfIndex != 0 {
		data = appendWireAttribute(data, unix.RTA_OIF, wireUint32(uint32(route.IfIndex)))
	}
	return data
}

func marshalOwnedRule(rule ownedRule) []byte {
	data := make([]byte, 12)
	data[0], data[7] = rule.Family, rule.Action
	if rule.Table != 0 {
		if rule.Table < 256 {
			data[4] = byte(rule.Table)
		}
		data = appendWireAttribute(data, unix.FRA_TABLE, wireUint32(rule.Table))
	}
	data = appendWireAttribute(data, unix.FRA_PRIORITY, wireUint32(rule.Priority))
	data = appendWireAttribute(data, unix.FRA_FWMARK, wireUint32(rule.Mark))
	data = appendWireAttribute(data, unix.FRA_FWMASK, wireUint32(^uint32(0)))
	return appendWireAttribute(data, unix.FRA_PROTOCOL, []byte{linuxOwnerProtocol})
}

func (s *linuxRouteSystem) removeRoute(ctx context.Context, observation *routeObservation, route ownedRoute) error {
	for _, actual := range observation.Routes {
		if !sameRoute(actual.ownedRoute, route) {
			continue
		}
		if !s.ownsRoute(actual) {
			return errors.New("managed route was changed externally; refusing deletion")
		}
		_, err := s.request.Execute(ctx, unix.RTM_DELROUTE, unix.NLM_F_ACK, marshalOwnedRoute(route))
		if err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
			return err
		}
		observation.changed = true
	}
	observation.Routes = slices.DeleteFunc(observation.Routes, func(actual observedRoute) bool { return sameRoute(actual.ownedRoute, route) })
	return s.updateState(func(state *routeState) {
		state.Routes = slices.DeleteFunc(state.Routes, func(old ownedRoute) bool { return sameRoute(old, route) })
	})
}

func (s *linuxRouteSystem) removeRule(ctx context.Context, observation *routeObservation, rule ownedRule) error {
	for _, actual := range observation.Rules {
		if !sameRule(actual.ownedRule, rule) {
			continue
		}
		if !s.ownsRule(actual) {
			return errors.New("managed rule was changed externally; refusing deletion")
		}
		_, err := s.request.Execute(ctx, unix.RTM_DELRULE, unix.NLM_F_ACK, marshalOwnedRule(rule))
		if err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
			return err
		}
		observation.changed = true
	}
	observation.Rules = slices.DeleteFunc(observation.Rules, func(actual observedRule) bool { return sameRule(actual.ownedRule, rule) })
	return s.updateState(func(state *routeState) {
		state.Rules = slices.DeleteFunc(state.Rules, func(old ownedRule) bool { return sameRule(old, rule) })
	})
}

// QuiesceTUN is a synchronous device-close fence. Merely scheduling an event
// would permit marked packets to escape while the old device was disappearing.
func (s *linuxRouteSystem) QuiesceTUN(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var policies []routePolicy
	for _, policy := range s.state.Policies {
		if policy.Backend == ModeTUN {
			policy.Ready = false
			policies = append(policies, policy)
		}
	}
	if len(policies) == 0 {
		return nil
	}
	if err := s.ensureRoutesLocked(ctx, policies); err != nil {
		return err
	}
	observation, err := s.routeSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := s.checkRouteConflicts(observation, policies); err != nil {
		return err
	}
	for _, policy := range policies {
		if err := s.verifyPolicyRoutes(observation, policy, 0); err != nil {
			return fmt.Errorf("TUN close refused: %w", err)
		}
	}
	return nil
}

// PruneRoutes is called only after the firewall has stopped using retired
// policy identities. It removes exact owned objects, never flushes a table.
func (s *linuxRouteSystem) PruneRoutes(ctx context.Context, policies []routePolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneRoutesLocked(ctx, policies)
}

func (s *linuxRouteSystem) pruneRoutesLocked(ctx context.Context, policies []routePolicy) error {
	observation, err := s.routeSnapshot(ctx)
	if err != nil {
		return err
	}
	keepRoute := func(route ownedRoute) bool {
		for _, policy := range policies {
			if route.Family != routeFamily(policy.IPv6) || route.Table != policy.Table {
				continue
			}
			if sameRoute(route, blackholeRoute(policy)) {
				return true
			}
			if policy.Ready && route.Metric == activeRouteMetric && (policy.Backend == ModeTProxy && route.Type == unix.RTN_LOCAL || policy.Backend == ModeTUN && route.Type == unix.RTN_UNICAST && route.IfIndex == policy.IfIndex) {
				return true
			}
		}
		return false
	}
	keepRule := func(rule ownedRule) bool {
		for _, policy := range policies {
			guard, lookup := policyRules(policy)
			if sameRule(rule, guard) || sameRule(rule, lookup) {
				return true
			}
		}
		return false
	}
	// Keep terminal guards through all other cleanup, including failure paths.
	for _, route := range slices.Clone(s.state.Routes) {
		if route.Type != unix.RTN_BLACKHOLE && !keepRoute(route) {
			if err := s.removeRoute(ctx, &observation, route); err != nil {
				return err
			}
		}
	}
	for _, rule := range slices.Clone(s.state.Rules) {
		if rule.Action != unix.FR_ACT_BLACKHOLE && !keepRule(rule) {
			if err := s.removeRule(ctx, &observation, rule); err != nil {
				return err
			}
		}
	}
	for _, route := range slices.Clone(s.state.Routes) {
		if !keepRoute(route) {
			if err := s.removeRoute(ctx, &observation, route); err != nil {
				return err
			}
		}
	}
	for _, rule := range slices.Clone(s.state.Rules) {
		if !keepRule(rule) {
			if err := s.removeRule(ctx, &observation, rule); err != nil {
				return err
			}
		}
	}
	return s.updateState(func(state *routeState) { state.Policies = slices.Clone(policies) })
}

func (s *linuxRouteSystem) CloseRoutes(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneRoutesLocked(ctx, nil)
}
