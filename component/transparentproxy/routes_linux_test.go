//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

type routeTestKernel struct {
	routes             []ownedRoute
	rules              []ownedRule
	links              []networkInterface
	writes             []wirePacket
	beforeWrite        func(wirePacket) error
	dropDumpProtection bool
}

func testLinkMessage(link networkInterface) wireMessage {
	data := make([]byte, 16)
	wireOrder.PutUint32(data[4:8], uint32(link.Index))
	flags := uint32(0)
	if link.Up {
		flags |= unix.IFF_UP
	}
	if link.Loopback {
		flags |= unix.IFF_LOOPBACK
	}
	wireOrder.PutUint32(data[8:12], flags)
	data = appendWireAttribute(data, unix.IFLA_IFNAME, append([]byte(link.Name), 0))
	return wireMessage{Type: unix.RTM_NEWLINK, Data: data}
}

func (k *routeTestKernel) handle(request wirePacket) []wirePacket {
	var messages []wireMessage
	switch request.Type {
	case unix.RTM_GETLINK:
		for _, link := range k.links {
			messages = append(messages, testLinkMessage(link))
		}
		return testWireDump(messages...)
	case unix.RTM_GETROUTE:
		for _, route := range k.routes {
			if k.dropDumpProtection && route.Type == unix.RTN_BLACKHOLE {
				continue
			}
			data := marshalOwnedRoute(route)
			if route.Family == unix.AF_INET6 {
				data[6] = unix.RT_SCOPE_UNIVERSE
				if route.Type == unix.RTN_BLACKHOLE {
					data = appendWireAttribute(data, unix.RTA_OIF, wireUint32(1))
				}
				data = appendWireAttribute(data, unix.RTA_PREF, []byte{0})
				data = appendWireAttribute(data, unix.RTA_CACHEINFO, make([]byte, 32))
			}
			messages = append(messages, wireMessage{Type: unix.RTM_NEWROUTE, Data: data})
		}
		return testWireDump(messages...)
	case unix.RTM_GETRULE:
		for _, family := range []byte{unix.AF_INET, unix.AF_INET6} {
			data := make([]byte, 12)
			data[0], data[4], data[7] = family, unix.RT_TABLE_LOCAL, unix.FR_ACT_TO_TBL
			data = appendWireAttribute(data, unix.FRA_PROTOCOL, []byte{unix.RTPROT_KERNEL})
			messages = append(messages, wireMessage{Type: unix.RTM_NEWRULE, Data: data})
		}
		for _, rule := range k.rules {
			messages = append(messages, wireMessage{Type: unix.RTM_NEWRULE, Data: marshalOwnedRule(rule)})
		}
		return testWireDump(messages...)
	case unix.RTM_NEWROUTE, unix.RTM_DELROUTE, unix.RTM_NEWRULE, unix.RTM_DELRULE:
		if k.beforeWrite != nil {
			if err := k.beforeWrite(request); err != nil {
				return []wirePacket{testWireAck(err)}
			}
		}
		k.writes = append(k.writes, request)
	default:
		panic("unexpected test request")
	}
	switch request.Type {
	case unix.RTM_NEWROUTE, unix.RTM_DELROUTE:
		observed, err := decodeObservedRoute(request.Data)
		if err != nil {
			panic(err)
		}
		route := observed.ownedRoute
		index := slices.IndexFunc(k.routes, func(old ownedRoute) bool { return sameRoute(old, route) })
		if request.Type == unix.RTM_NEWROUTE {
			if index >= 0 {
				return []wirePacket{testWireAck(unix.EEXIST)}
			}
			k.routes = append(k.routes, route)
		} else if index >= 0 {
			k.routes = slices.Delete(k.routes, index, index+1)
		}
	case unix.RTM_NEWRULE, unix.RTM_DELRULE:
		observed, err := decodeObservedRule(request.Data)
		if err != nil {
			panic(err)
		}
		rule := observed.ownedRule
		index := slices.IndexFunc(k.rules, func(old ownedRule) bool { return sameRule(old, rule) })
		if request.Type == unix.RTM_NEWRULE {
			if index >= 0 {
				return []wirePacket{testWireAck(unix.EEXIST)}
			}
			k.rules = append(k.rules, rule)
		} else if index >= 0 {
			k.rules = slices.Delete(k.rules, index, index+1)
		}
	}
	return []wirePacket{testWireAck(nil)}
}

func routeTestSystem(t *testing.T) (*linuxRouteSystem, *routeTestKernel, *int) {
	t.Helper()
	kernel := &routeTestKernel{links: []networkInterface{{Index: 1, Name: "lo", Up: true, Loopback: true}, {Index: 10, Name: "utun", Up: true}}}
	writes := new(int)
	system := &linuxRouteSystem{request: testWireConnection(kernel.handle), lease: -1, procRoot: t.TempDir(), writeState: func() error { *writes++; return nil }}
	t.Cleanup(func() { _ = system.Close() })
	writeTestSysctl(t, system, "net/ipv6/conf/all/disable_ipv6", "0")
	return system, kernel, writes
}

func writeTestSysctl(t *testing.T, system *linuxRouteSystem, path, value string) {
	t.Helper()
	full := filepath.Join(system.procRoot, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testTUNPolicies() []routePolicy {
	return []routePolicy{{Backend: ModeTUN, Mark: 0x162, Table: 101, Priority: 1010, IfIndex: 10, Ready: true}, {Backend: ModeTUN, Mark: 0x162, Table: 101, Priority: 1010, IfIndex: 10, Ready: true, IPv6: true}}
}

func TestRoutesProtectBeforeForwardingAndDoNotRewriteUnchangedState(t *testing.T) {
	s, kernel, persisted := routeTestSystem(t)
	policies := testTUNPolicies()
	kernel.beforeWrite = func(request wirePacket) error {
		switch request.Type {
		case unix.RTM_NEWROUTE:
			route, err := decodeObservedRoute(request.Data)
			if err != nil || !s.ownsRoute(route) {
				t.Fatalf("route mutation preceded durable ownership: %+v, %v", route, err)
			}
			if route.Type != unix.RTN_BLACKHOLE {
				for _, policy := range policies {
					guard, _ := policyRules(policy)
					if !slices.ContainsFunc(kernel.routes, func(r ownedRoute) bool { return sameRoute(r, blackholeRoute(policy)) }) || !slices.ContainsFunc(kernel.rules, func(r ownedRule) bool { return sameRule(r, guard) }) {
						t.Fatal("forwarding installed before both families were protected")
					}
				}
			}
		case unix.RTM_NEWRULE:
			rule, err := decodeObservedRule(request.Data)
			if err != nil || !s.ownsRule(rule) {
				t.Fatalf("rule mutation preceded durable ownership: %+v, %v", rule, err)
			}
		}
		return nil
	}
	if err := s.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	if len(kernel.routes) != 4 || len(kernel.rules) != 4 {
		t.Fatalf("bad dual-stack resources: %+v %+v", kernel.routes, kernel.rules)
	}
	writes, diskWrites := len(kernel.writes), *persisted
	if err := s.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	if len(kernel.writes) != writes || *persisted != diskWrites {
		t.Fatalf("unchanged event wrote kernel/disk: %d->%d %d->%d", writes, len(kernel.writes), diskWrites, *persisted)
	}
}

func TestRoutesQuiesceAndRebindKeepPermanentProtection(t *testing.T) {
	s, kernel, _ := routeTestSystem(t)
	policies := testTUNPolicies()
	if err := s.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	if err := s.QuiesceTUN(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kernel.routes) != 2 || slices.ContainsFunc(kernel.routes, func(route ownedRoute) bool { return route.Type != unix.RTN_BLACKHOLE }) {
		t.Fatalf("TUN close fence left forwarding: %+v", kernel.routes)
	}
	if len(kernel.rules) != 4 {
		t.Fatal("close fence removed a guard or lookup")
	}
	kernel.links[1].Index = 20
	for i := range policies {
		policies[i].IfIndex = 20
	}
	if err := s.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(kernel.routes, func(route ownedRoute) bool { return route.Type != unix.RTN_BLACKHOLE && route.IfIndex != 20 }) {
		t.Fatal("rebind used the old interface index")
	}
	// An unexpected device deletion drops only its unicast routes. The
	// independently keyed defaults still prevent a lookup from reaching main.
	kernel.routes = slices.DeleteFunc(kernel.routes, func(route ownedRoute) bool { return route.IfIndex == 20 })
	if len(kernel.routes) != 2 {
		t.Fatal("permanent blackhole depended on TUN index")
	}
	_, lookup := policyRules(policies[0])
	kernel.rules = slices.DeleteFunc(kernel.rules, func(rule ownedRule) bool { return sameRule(rule, lookup) })
	for i := range policies {
		policies[i].Ready = false
	}
	if err := s.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	if len(kernel.rules) != 4 {
		t.Fatal("missing lookup was not restored")
	}
}

func TestRoutesRefuseCloseWhenProtectionCannotBeVerified(t *testing.T) {
	s, kernel, _ := routeTestSystem(t)
	policies := testTUNPolicies()
	if err := s.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	guard, _ := policyRules(policies[0])
	kernel.rules = slices.DeleteFunc(kernel.rules, func(rule ownedRule) bool { return sameRule(rule, guard) })
	kernel.beforeWrite = func(request wirePacket) error {
		if request.Type == unix.RTM_NEWRULE {
			return unix.EPERM
		}
		return nil
	}
	if err := s.QuiesceTUN(context.Background()); !errors.Is(err, unix.EPERM) {
		t.Fatalf("unprotected close result: %v", err)
	}
	if len(kernel.routes) != 4 {
		t.Fatal("removed live routes before restoring the missing protection")
	}
	kernel.beforeWrite = nil
	kernel.dropDumpProtection = true
	if err := s.QuiesceTUN(context.Background()); err == nil {
		t.Fatal("accepted unverifiable protection")
	}
}

func TestRoutesDoNotAdoptExternalTableOrPriorityObjects(t *testing.T) {
	s, _, _ := routeTestSystem(t)
	policy := testTUNPolicies()[0]
	route := blackholeRoute(policy)
	for _, observation := range []routeObservation{
		{Routes: []observedRoute{{ownedRoute: route, Protocol: linuxOwnerProtocol, Plain: true, Default: true}}},
		{Rules: []observedRule{{ownedRule: ownedRule{Family: unix.AF_INET, Table: 200, Priority: 500, Mark: 0x999, Action: unix.FR_ACT_TO_TBL}, MarkSet: true, Mask: ^uint32(0), Plain: false}}},
		{Rules: []observedRule{{ownedRule: ownedRule{Family: unix.AF_INET, Table: 200, Priority: 0, Action: unix.FR_ACT_TO_TBL}, Plain: true}}},
		{Rules: []observedRule{{ownedRule: ownedRule{Family: unix.AF_INET, Table: 200, Priority: 1011, Mark: 0x999, Action: unix.FR_ACT_TO_TBL}, MarkSet: true, Mask: ^uint32(0), Plain: true}}},
	} {
		if err := s.checkRouteConflicts(observation, []routePolicy{policy}); err == nil {
			t.Fatalf("accepted an external conflict: %+v", observation)
		}
	}
}

func TestRoutesPersistFailureCannotAuthorizeKernelMutation(t *testing.T) {
	s, kernel, _ := routeTestSystem(t)
	s.writeState = func() error { return os.ErrPermission }
	if err := s.EnsureRoutes(context.Background(), testTUNPolicies()); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if len(kernel.writes) != 0 || len(s.manifest.Policies) != 0 || len(s.manifest.Routes) != 0 {
		t.Fatal("failed journal left applied or claimed resources")
	}
}

func TestRoutesUnavailableIPv6KeepsIntentAndRefusesUnprotectedClose(t *testing.T) {
	s, kernel, _ := routeTestSystem(t)
	writeTestSysctl(t, s, "net/ipv6/conf/all/disable_ipv6", "1")
	if err := s.EnsureRoutes(context.Background(), testTUNPolicies()); err != nil {
		t.Fatal(err)
	}
	if len(s.manifest.Policies) != 2 || s.manifest.Policies[1].Ready {
		t.Fatalf("IPv6 intent lost: %+v", s.manifest.Policies)
	}
	if slices.ContainsFunc(kernel.routes, func(route ownedRoute) bool { return route.Family == unix.AF_INET6 }) {
		t.Fatal("configured unavailable IPv6")
	}
	if err := s.QuiesceTUN(context.Background()); err == nil {
		t.Fatal("close accepted absent IPv6 protection")
	}
	value, _ := s.readSysctl("net/ipv6/conf/all/disable_ipv6")
	if value != "1" {
		t.Fatal("overrode administrator IPv6 disable")
	}
}

func TestNetworkAddressUsesValidityNotPreferredLifetime(t *testing.T) {
	makeAddress := func(family byte, address string, bits byte, flags, preferred, valid uint32) []byte {
		data := make([]byte, 8)
		data[0], data[1] = family, bits
		wireOrder.PutUint32(data[4:8], 10)
		data = appendWireAttribute(data, unix.IFA_ADDRESS, netip.MustParseAddr(address).AsSlice())
		data = appendWireAttribute(data, unix.IFA_FLAGS, wireUint32(flags))
		lifetime := make([]byte, 16)
		wireOrder.PutUint32(lifetime[:4], preferred)
		wireOrder.PutUint32(lifetime[4:8], valid)
		return appendWireAttribute(data, unix.IFA_CACHEINFO, lifetime)
	}
	for _, test := range []struct {
		flags, preferred, valid uint32
		usable                  bool
	}{
		{0, 0, ^uint32(0), true},
		{unix.IFA_F_DEPRECATED, 0, 20, true},
		{unix.IFA_F_TENTATIVE, 20, 20, false},
		{unix.IFA_F_DADFAILED, 20, 20, false},
		{0, 0, 0, false},
	} {
		_, prefix, usable, err := decodeNetworkAddress(makeAddress(unix.AF_INET6, "2001:db8:7::9", 56, test.flags, test.preferred, test.valid))
		if err != nil || usable != test.usable || prefix.String() != "2001:db8:7::9/56" {
			t.Fatalf("address validity: %s %v %v", prefix, usable, err)
		}
	}
	data := makeAddress(unix.AF_INET, "192.0.2.1", 27, 0, ^uint32(0), ^uint32(0))
	data = appendWireAttribute(data, unix.IFA_LOCAL, netip.MustParseAddr("192.0.2.2").AsSlice())
	_, prefix, usable, err := decodeNetworkAddress(data)
	if err != nil || !usable || prefix.String() != "192.0.2.2/27" {
		t.Fatalf("IPv4 peer/local distinction: %s %v %v", prefix, usable, err)
	}
}
