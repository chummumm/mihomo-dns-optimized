//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

func freshRouteTestProcess(t *testing.T, previous *linuxRouteSystem, kernel *routeTestKernel) *linuxRouteSystem {
	t.Helper()
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	next := &linuxRouteSystem{request: testWireConnection(kernel.handle), procRoot: previous.procRoot, lease: -1}
	t.Cleanup(func() { _ = next.Close() })
	return next
}

func TestRoutesRecoverSameConfigurationWithoutProcessState(t *testing.T) {
	for _, loseLookup := range []bool{false, true} {
		t.Run(map[bool]string{false: "blackholes-retained", true: "lookup-also-lost"}[loseLookup], func(t *testing.T) {
			old, kernel, _ := routeTestSystem(t)
			policies := testTUNPolicies()
			if err := old.EnsureRoutes(context.Background(), policies); err != nil {
				t.Fatal(err)
			}
			// An unexpected exit closes the non-persistent TUN, but the default
			// blackholes and policy rules remain independently of its ifindex.
			kernel.routes = slices.DeleteFunc(kernel.routes, func(route ownedRoute) bool { return route.Type == unix.RTN_UNICAST })
			if loseLookup {
				_, lookup := policyRules(policies[0])
				kernel.rules = slices.DeleteFunc(kernel.rules, func(rule ownedRule) bool { return sameRule(rule, lookup) })
			}
			for i := range policies {
				policies[i].Ready, policies[i].IfIndex = false, 0
			}
			next := freshRouteTestProcess(t, old, kernel)
			before := len(kernel.writes)
			if err := next.RecoverRoutes(context.Background(), policies, nil); err != nil {
				t.Fatal(err)
			}
			if len(kernel.writes) != before {
				t.Fatal("read-only recovery changed working routes")
			}
			if err := next.EnsureRoutes(context.Background(), policies); err != nil {
				t.Fatal(err)
			}
			if len(kernel.routes) != 2 || len(kernel.rules) != 4 {
				t.Fatalf("same-configuration recovery: %+v %+v", kernel.routes, kernel.rules)
			}
			kernel.links[1].Index = 20
			kernel.tunTypes[20] = unix.IFF_TUN
			for i := range policies {
				policies[i].Ready, policies[i].IfIndex = true, 20
			}
			if err := next.EnsureRoutes(context.Background(), policies); err != nil {
				t.Fatal(err)
			}
			if len(kernel.routes) != 4 || len(kernel.rules) != 4 {
				t.Fatal("replacement created duplicate or incomplete resources")
			}
			if err := next.CloseRoutes(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(kernel.routes) != 0 || len(kernel.rules) != 0 {
				t.Fatal("normal stop did not remove the recovered resources")
			}
		})
	}
}

func TestRoutesRecoverOldCaptureGroupsAndLeaveUnrelatedProtocolResources(t *testing.T) {
	old, kernel, _ := routeTestSystem(t)
	previous := testTUNPolicies()
	for _, ipv6 := range []bool{false, true} {
		previous = append(previous, routePolicy{Backend: ModeTProxy, Mark: 1, Table: 100, Priority: 1000, IPv6: ipv6, Ready: true})
	}
	if err := old.EnsureRoutes(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	// This complete canonical group also uses protocol 197. Its mark is not
	// referenced by our actual capture rules, so it must not become ours.
	unrelated := routePolicy{Backend: ModeTUN, Mark: 0x999, Table: 199, Priority: 500}
	guard, lookup := policyRules(unrelated)
	kernel.routes = append(kernel.routes, blackholeRoute(unrelated))
	kernel.rules = append(kernel.rules, guard, lookup)
	current := testTUNPolicies()
	for i := range current {
		current[i].Mark, current[i].Table, current[i].Priority = 0x163, 111, 1110
		current[i].Ready = false
	}
	var scopes []captureScope
	for _, policy := range previous {
		scopes = append(scopes, captureScope{Backend: policy.Backend, Mark: policy.Mark, IPv6: policy.IPv6})
	}
	next := freshRouteTestProcess(t, old, kernel)
	if err := next.RecoverRoutes(context.Background(), current, scopes); err != nil {
		t.Fatal(err)
	}
	if len(next.state.Policies) != len(previous)+len(current) || len(next.state.Routes) != 8 || len(next.state.Rules) != 8 {
		t.Fatalf("working old groups not recovered exactly: %+v", next.state)
	}
	if err := next.EnsureRoutes(context.Background(), current); err != nil {
		t.Fatalf("unrelated retained group blocked the new configuration: %v", err)
	}
	// The manager switches nft capture before pruning its old route identities.
	if err := next.PruneRoutes(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(kernel.routes, func(route ownedRoute) bool { return route.Table == 100 || route.Table == 101 }) || slices.ContainsFunc(kernel.rules, func(rule ownedRule) bool { return rule.Mark == 1 || rule.Mark == 0x162 }) {
		t.Fatal("changed configuration retained identifiable old resources")
	}
	if err := next.CloseRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kernel.routes) != 1 || !sameRoute(kernel.routes[0], blackholeRoute(unrelated)) || len(kernel.rules) != 2 {
		t.Fatalf("normal stop changed unrelated protocol resources: %+v %+v", kernel.routes, kernel.rules)
	}
}

func TestRoutesRecoveryLeavesUnidentifiableOldGroupWithoutBlockingOtherMark(t *testing.T) {
	old, kernel, _ := routeTestSystem(t)
	previous := testTUNPolicies()[0]
	previous.Ready = false
	if err := old.EnsureRoutes(context.Background(), []routePolicy{previous}); err != nil {
		t.Fatal(err)
	}
	_, lookup := policyRules(previous)
	kernel.rules = slices.DeleteFunc(kernel.rules, func(rule ownedRule) bool { return sameRule(rule, lookup) })
	current := previous
	current.Mark, current.Table, current.Priority = 0x163, 111, 1110
	next := freshRouteTestProcess(t, old, kernel)
	if err := next.RecoverRoutes(context.Background(), []routePolicy{current}, []captureScope{{Backend: ModeTUN, Mark: previous.Mark}}); err != nil {
		t.Fatal(err)
	}
	if len(next.state.Routes) != 0 || len(next.state.Rules) != 0 {
		t.Fatal("missing old lookup was used to guess an unknown table")
	}
	if err := next.EnsureRoutes(context.Background(), []routePolicy{current}); err != nil {
		t.Fatal(err)
	}
	if err := next.CloseRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kernel.routes) != 1 || len(kernel.rules) != 1 || kernel.rules[0].Mark != previous.Mark {
		t.Fatal("unidentifiable old protection was removed")
	}
}

func TestRoutesRecoveryRequiresExactRouteAndActualBackendDevice(t *testing.T) {
	for _, test := range []string{"foreign-protocol", "extra-gateway", "wrong-metric", "ordinary-interface", "tap-interface", "local-through-nonloopback"} {
		t.Run(test, func(t *testing.T) {
			s, kernel, _ := routeTestSystem(t)
			policy := testTUNPolicies()[0]
			route := activeRoute(policy, 1)
			switch test {
			case "foreign-protocol":
				data := marshalOwnedRoute(blackholeRoute(policy))
				data[5] = unix.RTPROT_STATIC
				kernel.extraRoutes = []wireMessage{{Type: unix.RTM_NEWROUTE, Data: data}}
			case "extra-gateway":
				data := appendWireAttribute(marshalOwnedRoute(route), unix.RTA_GATEWAY, []byte{192, 0, 2, 1})
				kernel.extraRoutes = []wireMessage{{Type: unix.RTM_NEWROUTE, Data: data}}
			case "wrong-metric":
				route.Metric++
				kernel.routes = []ownedRoute{route}
			case "ordinary-interface":
				delete(kernel.tunTypes, 10)
				kernel.routes = []ownedRoute{route}
			case "tap-interface":
				kernel.tunTypes[10] = unix.IFF_TAP
				kernel.routes = []ownedRoute{route}
			case "local-through-nonloopback":
				policy.Backend = ModeTProxy
				route = activeRoute(policy, 10)
				kernel.routes = []ownedRoute{route}
			}
			if err := s.RecoverRoutes(context.Background(), []routePolicy{policy}, nil); err != nil {
				t.Fatal(err)
			}
			if len(s.state.Routes) != 0 {
				t.Fatalf("claimed an external route: %+v", s.state.Routes)
			}
			if err := s.EnsureRoutes(context.Background(), []routePolicy{policy}); err == nil {
				t.Fatal("capture was allowed through a conflicting table")
			}
			if err := s.CloseRoutes(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(kernel.writes) != 0 {
				t.Fatal("conflict detection or stop mutated an external route")
			}
		})
	}
}

func TestRoutesRecoveryWaitsForCompleteKernelSnapshot(t *testing.T) {
	for _, failed := range []uint16{unix.RTM_GETROUTE, unix.RTM_GETRULE, unix.RTM_GETLINK} {
		s, kernel, _ := routeTestSystem(t)
		policy := testTUNPolicies()[0]
		kernel.routes = append(kernel.routes, blackholeRoute(policy))
		guard, lookup := policyRules(policy)
		kernel.rules = append(kernel.rules, guard, lookup)
		kernel.failDump = failed
		if err := s.RecoverRoutes(context.Background(), []routePolicy{policy}, nil); !errors.Is(err, unix.EIO) {
			t.Fatalf("incomplete snapshot accepted: %v", err)
		}
		if len(s.state.Policies)+len(s.state.Routes)+len(s.state.Rules) != 0 || len(kernel.writes) != 0 {
			t.Fatal("incomplete snapshot authorized resource ownership")
		}
	}
}

func TestRoutesNormalStopCanRetryFailedCleanup(t *testing.T) {
	s, kernel, _ := routeTestSystem(t)
	if err := s.EnsureRoutes(context.Background(), testTUNPolicies()); err != nil {
		t.Fatal(err)
	}
	kernel.beforeWrite = func(request wirePacket) error {
		if request.Type == unix.RTM_DELRULE {
			return unix.EPERM
		}
		return nil
	}
	if err := s.CloseRoutes(context.Background()); !errors.Is(err, unix.EPERM) {
		t.Fatal(err)
	}
	if len(s.state.Policies) != 2 || len(s.state.Rules) != 4 || len(kernel.routes) != 2 {
		t.Fatal("failed stop lost remaining guards or their runtime ownership")
	}
	kernel.beforeWrite = nil
	if err := s.CloseRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kernel.routes)+len(kernel.rules)+len(s.state.Routes)+len(s.state.Rules)+len(s.state.Policies) != 0 {
		t.Fatal("retry did not completely clean the module routes")
	}
}
