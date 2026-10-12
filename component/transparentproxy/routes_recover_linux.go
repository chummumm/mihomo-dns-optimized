//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

// RecoverRoutes learns only actual working objects. The configured exact
// identities reserve this module's protocol/table/priority namespace; retired
// identities additionally require a reachable nft capture mark and its complete
// lookup/terminal-guard/default-blackhole group. Protocol 197 alone never
// authorizes a table scan or removal. Nothing survives outside these working
// objects and this process's memory.
func (s *linuxRouteSystem) RecoverRoutes(ctx context.Context, configured []routePolicy, captures []captureScope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	policies := slices.Clone(configured)
	for i := range policies {
		policies[i].Ready = false
		if err := validateRoutePolicy(policies[i]); err != nil {
			return err
		}
	}
	for _, scope := range captures {
		if scope.Backend != ModeTUN && scope.Backend != ModeTProxy || scope.Mark == 0 || scope.Mark == DefaultOutboundMark {
			return errors.New("invalid working capture scope")
		}
	}
	observation, err := s.routeSnapshot(ctx)
	if err != nil {
		return err
	}
	links := observation.Links
	if links == nil {
		read, err := s.readRouteLinks(ctx)
		if err != nil {
			return err
		}
		links = &read
	}
	for _, scope := range captures {
		// One mark cannot prove two different backends. A partially modified
		// firewall is not permission to guess which old table belongs to which.
		if slices.ContainsFunc(captures, func(other captureScope) bool {
			return other.IPv6 == scope.IPv6 && other.Mark == scope.Mark && other.Backend != scope.Backend
		}) {
			continue
		}
		for _, rule := range observation.Rules {
			if !canonicalModuleRule(rule) || rule.Action != unix.FR_ACT_TO_TBL || rule.Family != routeFamily(scope.IPv6) || rule.Mark != scope.Mark {
				continue
			}
			policy := routePolicy{Backend: scope.Backend, Mark: rule.Mark, Table: rule.Table, Priority: rule.Priority, IPv6: scope.IPv6}
			if err := validateRoutePolicy(policy); err != nil {
				continue
			}
			guard, _ := policyRules(policy)
			if !slices.ContainsFunc(observation.Rules, func(actual observedRule) bool {
				return canonicalModuleRule(actual) && sameRule(actual.ownedRule, guard)
			}) || !slices.ContainsFunc(observation.Routes, func(actual observedRoute) bool {
				return canonicalModuleRoute(actual) && sameRoute(actual.ownedRoute, blackholeRoute(policy))
			}) {
				continue
			}
			if !slices.ContainsFunc(policies, func(old routePolicy) bool { return samePolicy(old, policy) }) {
				policies = append(policies, policy)
			}
		}
	}
	return s.updateState(func(state *routeState) {
		for _, policy := range policies {
			if !slices.ContainsFunc(state.Policies, func(old routePolicy) bool { return samePolicy(old, policy) }) {
				state.Policies = append(state.Policies, policy)
			}
			for _, actual := range observation.Routes {
				if !canonicalModuleRoute(actual) || actual.Family != routeFamily(policy.IPv6) || actual.Table != policy.Table {
					continue
				}
				owned := actual.ownedRoute
				owned.Backend = policy.Backend
				if !sameRoute(owned, blackholeRoute(policy)) {
					wanted := policy
					wanted.IfIndex = owned.IfIndex
					if !sameRoute(owned, activeRoute(wanted, links.Loopback)) || owned.IfIndex <= 0 {
						continue
					}
					if policy.Backend == ModeTUN && !links.TUN[owned.IfIndex] {
						continue
					}
				}
				if !slices.ContainsFunc(state.Routes, func(old ownedRoute) bool { return sameRoute(old, owned) }) {
					state.Routes = append(state.Routes, owned)
				}
			}
			guard, lookup := policyRules(policy)
			for _, actual := range observation.Rules {
				if !canonicalModuleRule(actual) || !sameRule(actual.ownedRule, guard) && !sameRule(actual.ownedRule, lookup) {
					continue
				}
				owned := actual.ownedRule
				owned.Backend = policy.Backend
				if !slices.ContainsFunc(state.Rules, func(old ownedRule) bool { return sameRule(old, owned) }) {
					state.Rules = append(state.Rules, owned)
				}
			}
		}
	})
}

func canonicalModuleRoute(route observedRoute) bool {
	return route.Protocol == linuxOwnerProtocol && route.Default && route.Plain
}

func canonicalModuleRule(rule observedRule) bool {
	return rule.Protocol == linuxOwnerProtocol && rule.MarkSet && rule.Mask == ^uint32(0) && rule.Plain
}

type routeLinkState struct {
	Loopback   int
	Interfaces map[string]int
	TUN        map[int]bool
}

// Recovering an active route or assessing an output-bound lookup requires the
// actual device kind, not just a familiar interface name.
func (s *linuxRouteSystem) readRouteLinks(ctx context.Context) (routeLinkState, error) {
	state := routeLinkState{Interfaces: make(map[string]int), TUN: make(map[int]bool)}
	messages, err := s.request.Execute(ctx, unix.RTM_GETLINK, unix.NLM_F_DUMP, make([]byte, 16))
	if err != nil {
		return state, fmt.Errorf("read links for route verification: %w", err)
	}
	seen := make(map[int]bool)
	for _, message := range messages {
		if message.Type != unix.RTM_NEWLINK {
			return state, fmt.Errorf("%w: unexpected route link message", errIncompleteNetlink)
		}
		link, err := decodeNetworkLink(message.Data)
		if err != nil {
			return state, err
		}
		if seen[link.Index] || state.Interfaces[link.Name] != 0 {
			return state, fmt.Errorf("%w: duplicate route link", errIncompleteNetlink)
		}
		seen[link.Index] = true
		state.Interfaces[link.Name] = link.Index
		if link.Name == "lo" && link.Loopback {
			state.Loopback = link.Index
		}
		attributes, err := parseWireAttributes(message.Data[16:])
		if err != nil {
			return state, err
		}
		linkInfoSeen := false
		for _, attribute := range attributes {
			if attribute.Type&wireAttributeTypeMask != unix.IFLA_LINKINFO {
				continue
			}
			if linkInfoSeen {
				return state, fmt.Errorf("%w: duplicate route link info", errIncompleteNetlink)
			}
			linkInfoSeen = true
			info, err := parseWireAttributes(attribute.Data)
			if err != nil {
				return state, err
			}
			kind, data := "", []byte(nil)
			for _, item := range info {
				switch item.Type & wireAttributeTypeMask {
				case unix.IFLA_INFO_KIND:
					if kind != "" {
						return state, fmt.Errorf("%w: duplicate route link kind", errIncompleteNetlink)
					}
					kind = string(item.Data)
				case unix.IFLA_INFO_DATA:
					if data != nil {
						return state, fmt.Errorf("%w: duplicate route link data", errIncompleteNetlink)
					}
					data = item.Data
				}
			}
			if kind != "tun\x00" {
				continue
			}
			details, err := parseWireAttributes(data)
			if err != nil {
				return state, err
			}
			typeSeen := false
			for _, item := range details {
				if item.Type&wireAttributeTypeMask != unix.IFLA_TUN_TYPE {
					continue
				}
				if typeSeen || len(item.Data) != 1 {
					return state, fmt.Errorf("%w: invalid route TUN type", errIncompleteNetlink)
				}
				typeSeen = true
				state.TUN[link.Index] = item.Data[0] == unix.IFF_TUN
			}
		}
	}
	return state, nil
}
