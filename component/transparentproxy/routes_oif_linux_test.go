//go:build linux

package transparentproxy

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

func nativeOIFTestRule(table, priority, flags uint32, name string) wireMessage {
	data := make([]byte, 12)
	data[0], data[7] = unix.AF_INET6, unix.FR_ACT_TO_TBL
	wireOrder.PutUint32(data[8:12], flags)
	data = appendWireAttribute(data, unix.FRA_TABLE, wireUint32(table))
	data = appendWireAttribute(data, unix.FRA_PRIORITY, wireUint32(priority))
	data = appendWireAttribute(data, unix.FRA_OIFNAME, append([]byte(name), 0))
	return wireMessage{Type: unix.RTM_NEWRULE, Data: data}
}

func nativeTUNTestRoute(table uint32, index int) wireMessage {
	data := marshalOwnedRoute(ownedRoute{Family: unix.AF_INET6, Table: table, Metric: 1024, Type: unix.RTN_UNICAST, Scope: unix.RT_SCOPE_UNIVERSE, IfIndex: index})
	data[5] = unix.RTPROT_BOOT
	return wireMessage{Type: unix.RTM_NEWROUTE, Data: data}
}

func nativeTUNTestGatewayRoute(table uint32, index int, gateway string) wireMessage {
	route := nativeTUNTestRoute(table, index)
	route.Data = appendWireAttribute(route.Data, unix.RTA_GATEWAY, netip.MustParseAddr(gateway).AsSlice())
	return route
}

func nativeHelperPolicies() []routePolicy {
	policies := testTUNPolicies()
	for _, ipv6 := range []bool{false, true} {
		policies = append(policies, routePolicy{Backend: ModeTProxy, Mark: 1, Table: 100, Priority: 1000, IPv6: ipv6, IfIndex: 1, Ready: true})
	}
	return policies
}

func TestRoutesOutputSelectorDecodingKeepsOwnershipStrict(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]byte) []byte
		only   bool
		bad    bool
	}{
		{name: "plain output selector", only: true},
		{name: "detached output selector", only: true, change: func(data []byte) []byte { wireOrder.PutUint32(data[8:12], unix.FIB_RULE_OIF_DETACHED); return data }},
		{name: "inverted", change: func(data []byte) []byte { wireOrder.PutUint32(data[8:12], unix.FIB_RULE_INVERT); return data }},
		{name: "source prefix", change: func(data []byte) []byte { data[2] = 64; return data }},
		{name: "traffic class", change: func(data []byte) []byte { data[3] = 1; return data }},
		{name: "mark", change: func(data []byte) []byte { return appendWireAttribute(data, unix.FRA_FWMARK, wireUint32(0x999)) }},
		{name: "mask without mark", change: func(data []byte) []byte { return appendWireAttribute(data, unix.FRA_FWMASK, wireUint32(0)) }},
		{name: "input selector", change: func(data []byte) []byte { return appendWireAttribute(data, unix.FRA_IIFNAME, []byte("eth0\x00")) }},
		{name: "suppression", change: func(data []byte) []byte { return appendWireAttribute(data, unix.FRA_SUPPRESS_PREFIXLEN, wireUint32(0)) }},
		{name: "unknown selector", change: func(data []byte) []byte { return appendWireAttribute(data, 0x3ffe, []byte{1}) }},
		{name: "duplicate output selector", bad: true, change: func(data []byte) []byte { return appendWireAttribute(data, unix.FRA_OIFNAME, []byte("utun\x00")) }},
		{name: "empty output selector", bad: true, change: func([]byte) []byte { return nativeOIFTestRule(2022, 999, 0, "").Data }},
		{name: "long output selector", bad: true, change: func([]byte) []byte { return nativeOIFTestRule(2022, 999, 0, strings.Repeat("x", unix.IFNAMSIZ)).Data }},
		{name: "embedded null", bad: true, change: func([]byte) []byte { return nativeOIFTestRule(2022, 999, 0, "ut\x00un").Data }},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := nativeOIFTestRule(2022, 999, 0, "utun").Data
			if test.change != nil {
				data = test.change(data)
			}
			rule, err := decodeObservedRule(data)
			if test.bad {
				if !errors.Is(err, errIncompleteNetlink) {
					t.Fatalf("malformed selector accepted: %+v %v", rule, err)
				}
				return
			}
			if err != nil || rule.OIFOnly != test.only || rule.Plain {
				t.Fatalf("output selector broadened ownership or lost conditions: %+v %v", rule, err)
			}
		})
	}
	owned := marshalOwnedRule(ownedRule{Family: unix.AF_INET6, Table: 101, Mark: 0x162, Priority: 1010, Action: unix.FR_ACT_TO_TBL})
	rule, err := decodeObservedRule(appendWireAttribute(owned, unix.FRA_OIFNAME, []byte("utun\x00")))
	if err != nil || canonicalModuleRule(rule) || rule.OIFOnly {
		t.Fatalf("output-qualified mark rule became owned: %+v %v", rule, err)
	}
}

func TestRoutesNativeIPv6TUNHelperDoesNotConflictOrBecomeOwned(t *testing.T) {
	for _, test := range []struct {
		name        string
		policies    []routePolicy
		table, pref uint32
		gateway     string
	}{
		{name: "mixed backends", policies: nativeHelperPolicies(), table: 2022, pref: 999},
		{name: "both protocols use TProxy", policies: nativeHelperPolicies()[2:], table: 2022, pref: 999},
		{name: "different native table and preference", policies: nativeHelperPolicies(), table: 4242, pref: 7},
		// sing-tun v0.4.27 Inet6GatewayAddr for the real stop fixture's
		// fdfe:dcba:9876::1/126 is ::2, even with auto-route disabled.
		{name: "native IPv6 gateway", policies: nativeHelperPolicies(), table: 2022, pref: 999, gateway: "fdfe:dcba:9876::2"},
		{name: "native gateway with both TProxy", policies: nativeHelperPolicies()[2:], table: 2022, pref: 999, gateway: "fdfe:dcba:9876::2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, kernel, _ := routeTestSystem(t)
			kernel.extraRoutes = []wireMessage{nativeTUNTestRoute(test.table, 10)}
			if test.gateway != "" {
				kernel.extraRoutes[0] = nativeTUNTestGatewayRoute(test.table, 10, test.gateway)
			}
			kernel.extraRules = []wireMessage{nativeOIFTestRule(test.table, test.pref, 0, "utun")}
			if err := s.RecoverRoutes(context.Background(), test.policies, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureRoutes(context.Background(), test.policies); err != nil {
				t.Fatalf("native TUN helper was mistaken for a WAN diversion: %v", err)
			}
			if slices.ContainsFunc(s.state.Routes, func(route ownedRoute) bool { return route.Table == test.table }) || slices.ContainsFunc(s.state.Rules, func(rule ownedRule) bool { return rule.Priority == test.pref }) {
				t.Fatal("compatibility exception claimed native resources")
			}
			if err := s.QuiesceTUN(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.CloseRoutes(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, write := range kernel.writes {
				switch write.Type {
				case unix.RTM_NEWROUTE, unix.RTM_DELROUTE:
					route, err := decodeObservedRoute(write.Data)
					if err != nil || route.Table == test.table {
						t.Fatalf("modified the native table: %+v %v", route, err)
					}
				case unix.RTM_NEWRULE, unix.RTM_DELRULE:
					rule, err := decodeObservedRule(write.Data)
					if err != nil || rule.Priority == test.pref {
						t.Fatalf("modified the native rule: %+v %v", rule, err)
					}
				}
			}
		})
	}
}

func TestRoutesNativeGatewayDecodingKeepsOwnershipStrict(t *testing.T) {
	message := nativeTUNTestGatewayRoute(101, 10, "fdfe:dcba:9876::2")
	message.Data[5] = linuxOwnerProtocol
	route, err := decodeObservedRoute(message.Data)
	if err != nil || route.Plain || !route.SinglePath || canonicalModuleRoute(route) || route.Gateway != netip.MustParseAddr("fdfe:dcba:9876::2") {
		t.Fatalf("gateway changed ownership or was not decoded: %+v %v", route, err)
	}
	for _, data := range [][]byte{
		appendWireAttribute(nativeTUNTestRoute(2022, 10).Data, unix.RTA_GATEWAY, make([]byte, 4)),
		appendWireAttribute(message.Data, unix.RTA_GATEWAY, netip.MustParseAddr("fdfe:dcba:9876::3").AsSlice()),
	} {
		if _, err := decodeObservedRoute(data); !errors.Is(err, errIncompleteNetlink) {
			t.Fatalf("malformed or duplicate gateway accepted: %v", err)
		}
	}
}

func TestRoutesNativeGatewayRejectsUnprovenPaths(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]byte) []byte
		reason string
	}{
		{name: "WAN gateway", reason: "dev 77 instead of TUN 10", change: func([]byte) []byte { return nativeTUNTestGatewayRoute(2022, 77, "fdfe:dcba:9876::2").Data }},
		{name: "unspecified", reason: "invalid IPv6 unicast gateway", change: func([]byte) []byte { return nativeTUNTestGatewayRoute(2022, 10, "::").Data }},
		{name: "mapped IPv4", reason: "invalid IPv6 unicast gateway", change: func([]byte) []byte { return nativeTUNTestGatewayRoute(2022, 10, "::ffff:192.0.2.1").Data }},
		{name: "multicast", reason: "invalid IPv6 unicast gateway", change: func([]byte) []byte { return nativeTUNTestGatewayRoute(2022, 10, "ff02::1").Data }},
		{name: "multipath", reason: "attribute 9", change: func(data []byte) []byte { return appendWireAttribute(data, unix.RTA_MULTIPATH, make([]byte, 8)) }},
		{name: "nexthop object", reason: "attribute 30", change: func(data []byte) []byte { return appendWireAttribute(data, 30, wireUint32(5)) }},
		{name: "encapsulation", reason: "attribute 22", change: func(data []byte) []byte { return appendWireAttribute(data, unix.RTA_ENCAP, nil) }},
		{name: "via", reason: "attribute 18", change: func(data []byte) []byte { return appendWireAttribute(data, unix.RTA_VIA, make([]byte, 18)) }},
		{name: "unknown attribute", reason: "attribute 16382", change: func(data []byte) []byte { return appendWireAttribute(data, 0x3ffe, nil) }},
		{name: "offloaded route", reason: "flags 0x4000", change: func(data []byte) []byte { wireOrder.PutUint32(data[8:12], unix.RTM_F_OFFLOAD); return data }},
		{name: "terminating route with gateway", reason: "terminating target route has a gateway", change: func(data []byte) []byte { data[7] = unix.RTN_PROHIBIT; return data }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, kernel, _ := routeTestSystem(t)
			kernel.extraRules = []wireMessage{nativeOIFTestRule(2022, 999, 0, "utun")}
			message := nativeTUNTestGatewayRoute(2022, 10, "fdfe:dcba:9876::2")
			message.Data = test.change(message.Data)
			kernel.extraRoutes = []wireMessage{message}
			err := s.EnsureRoutes(context.Background(), nativeHelperPolicies())
			if err == nil || !strings.Contains(err.Error(), test.reason) || !strings.Contains(err.Error(), "table 2022, oif \"utun\"") {
				t.Fatalf("unsafe gateway path accepted or diagnostic lost: %v", err)
			}
			if len(kernel.writes) != 0 {
				t.Fatal("unsafe gateway lookup was detected after route mutation")
			}
		})
	}
}

func TestRoutesNativeHelperDoesNotHideUnsafeRoutesOrSelectors(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*routeTestKernel)
	}{
		{name: "ordinary interface", change: func(k *routeTestKernel) { delete(k.tunTypes, 10) }},
		{name: "TAP", change: func(k *routeTestKernel) { k.tunTypes[10] = unix.IFF_TAP }},
		{name: "WAN default", change: func(k *routeTestKernel) { k.extraRoutes[0] = nativeTUNTestRoute(2022, 77) }},
		{name: "WAN more specific", change: func(k *routeTestKernel) {
			route := nativeTUNTestRoute(2022, 77)
			route.Data[1] = 64
			route.Data = appendWireAttribute(route.Data, unix.RTA_DST, []byte{0x20, 1, 0x48, 0x60, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
			k.extraRoutes = append(k.extraRoutes, route)
		}},
		{name: "different TUN", change: func(k *routeTestKernel) {
			k.links = append(k.links, networkInterface{Index: 30, Name: "other-tun", Up: true})
			k.tunTypes[30] = unix.IFF_TUN
			k.extraRoutes[0] = nativeTUNTestRoute(2022, 30)
		}},
		{name: "gateway", change: func(k *routeTestKernel) {
			k.extraRoutes[0].Data = appendWireAttribute(k.extraRoutes[0].Data, unix.RTA_GATEWAY, make([]byte, 16))
		}},
		{name: "multipath", change: func(k *routeTestKernel) {
			k.extraRoutes[0].Data = appendWireAttribute(k.extraRoutes[0].Data, unix.RTA_MULTIPATH, make([]byte, 8))
		}},
		{name: "nexthop object", change: func(k *routeTestKernel) {
			// RTA_NH_ID is 30; the pinned x/sys does not expose its name.
			k.extraRoutes[0].Data = appendWireAttribute(k.extraRoutes[0].Data, 30, wireUint32(5))
		}},
		{name: "encapsulation", change: func(k *routeTestKernel) {
			k.extraRoutes[0].Data = appendWireAttribute(k.extraRoutes[0].Data, unix.RTA_ENCAP, nil)
		}},
		{name: "unknown route attribute", change: func(k *routeTestKernel) {
			k.extraRoutes[0].Data = appendWireAttribute(k.extraRoutes[0].Data, 0x3ffe, []byte{1})
		}},
		{name: "unknown route flags", change: func(k *routeTestKernel) { wireOrder.PutUint32(k.extraRoutes[0].Data[8:12], 0x80000000) }},
		{name: "linkdown with changed next-hop semantics", change: func(k *routeTestKernel) {
			wireOrder.PutUint32(k.extraRoutes[0].Data[8:12], unix.RTNH_F_LINKDOWN|unix.RTNH_F_ONLINK)
		}},
		{name: "throw route", change: func(k *routeTestKernel) { k.extraRoutes[0].Data[7] = unix.RTN_THROW }},
		{name: "local route", change: func(k *routeTestKernel) { k.extraRoutes[0].Data[7] = unix.RTN_LOCAL }},
		{name: "lookup preference collision", change: func(k *routeTestKernel) { k.extraRules[0] = nativeOIFTestRule(2022, 1000, 0, "utun") }},
		{name: "guard preference collision", change: func(k *routeTestKernel) { k.extraRules[0] = nativeOIFTestRule(2022, 1001, 0, "utun") }},
		{name: "reserved table", change: func(k *routeTestKernel) {
			k.extraRules[0] = nativeOIFTestRule(254, 999, 0, "utun")
			k.extraRoutes = nil
		}},
		{name: "managed table", change: func(k *routeTestKernel) {
			k.extraRules[0] = nativeOIFTestRule(101, 999, 0, "utun")
			k.extraRoutes = nil
		}},
		{name: "mark selector", change: func(k *routeTestKernel) {
			k.extraRules[0].Data = appendWireAttribute(k.extraRules[0].Data, unix.FRA_FWMARK, wireUint32(0x999))
		}},
		{name: "input selector", change: func(k *routeTestKernel) {
			k.extraRules[0].Data = appendWireAttribute(k.extraRules[0].Data, unix.FRA_IIFNAME, []byte("eth0\x00"))
		}},
		{name: "inverted output selector", change: func(k *routeTestKernel) { wireOrder.PutUint32(k.extraRules[0].Data[8:12], unix.FIB_RULE_INVERT) }},
		{name: "unknown selector", change: func(k *routeTestKernel) {
			k.extraRules[0].Data = appendWireAttribute(k.extraRules[0].Data, 0x3ffe, []byte{1})
		}},
		{name: "nonlookup action", change: func(k *routeTestKernel) { k.extraRules[0].Data[7] = unix.FR_ACT_GOTO }},
		{name: "detached flag on present device", change: func(k *routeTestKernel) { wireOrder.PutUint32(k.extraRules[0].Data[8:12], unix.FIB_RULE_OIF_DETACHED) }},
		{name: "missing device without detached flag", change: func(k *routeTestKernel) { k.links = k.links[:1]; k.extraRoutes = nil }},
		{name: "detached device but nonempty table", change: func(k *routeTestKernel) {
			k.links = k.links[:1]
			wireOrder.PutUint32(k.extraRules[0].Data[8:12], unix.FIB_RULE_OIF_DETACHED)
		}},
		{name: "IPv4 does not use the native exception", change: func(k *routeTestKernel) { k.extraRules[0].Data[0] = unix.AF_INET; k.extraRoutes = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, kernel, _ := routeTestSystem(t)
			kernel.extraRules = []wireMessage{nativeOIFTestRule(2022, 999, 0, "utun")}
			kernel.extraRoutes = []wireMessage{nativeTUNTestRoute(2022, 10)}
			test.change(kernel)
			if err := s.EnsureRoutes(context.Background(), nativeHelperPolicies()); err == nil {
				t.Fatal("unsafe native-looking rule bypassed conflict detection")
			}
			if len(kernel.writes) != 0 {
				t.Fatal("unsafe lookup was detected after route mutation")
			}
		})
	}
}

func TestRoutesNativeHelperRecoversDetachedEmptyTableAndNewTUN(t *testing.T) {
	old, kernel, _ := routeTestSystem(t)
	policies := nativeHelperPolicies()
	if err := old.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	kernel.routes = slices.DeleteFunc(kernel.routes, func(route ownedRoute) bool { return route.IfIndex == 10 })
	kernel.links = kernel.links[:1]
	kernel.extraRules = []wireMessage{nativeOIFTestRule(2022, 999, unix.FIB_RULE_OIF_DETACHED, "utun")}
	for i := range policies {
		if policies[i].Backend == ModeTUN {
			policies[i].Ready, policies[i].IfIndex = false, 0
		}
	}
	next := freshRouteTestProcess(t, old, kernel)
	if err := next.RecoverRoutes(context.Background(), policies, nil); err != nil {
		t.Fatal(err)
	}
	if err := next.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatalf("detached native helper blocked replacement startup: %v", err)
	}
	kernel.links = append(kernel.links, networkInterface{Index: 20, Name: "utun", Up: true})
	kernel.tunTypes[20] = unix.IFF_TUN
	kernel.extraRoutes = []wireMessage{nativeTUNTestRoute(2022, 20)}
	// Linux reattaches the old output-name rule. The fresh native listener also
	// installs its own helper; neither rule belongs to our cleanup scope.
	kernel.extraRules = []wireMessage{nativeOIFTestRule(2022, 999, 0, "utun"), nativeOIFTestRule(2022, 998, 0, "utun")}
	for i := range policies {
		if policies[i].Backend == ModeTUN {
			policies[i].Ready, policies[i].IfIndex = true, 20
		}
	}
	if err := next.EnsureRoutes(context.Background(), policies); err != nil {
		t.Fatal(err)
	}
	if err := next.QuiesceTUN(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := next.CloseRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(kernel.routes) != 0 || len(kernel.rules) != 0 || len(kernel.extraRules) != 2 || len(kernel.extraRoutes) != 1 {
		t.Fatal("managed cleanup removed native helpers or retained its own resources")
	}
}

func TestRoutesNativeHelperRequiresCompleteLinkObservation(t *testing.T) {
	s, kernel, _ := routeTestSystem(t)
	kernel.extraRules = []wireMessage{nativeOIFTestRule(2022, 999, 0, "utun")}
	kernel.extraRoutes = []wireMessage{nativeTUNTestRoute(2022, 10)}
	kernel.failDump = unix.RTM_GETLINK
	if err := s.EnsureRoutes(context.Background(), nativeHelperPolicies()); !errors.Is(err, unix.EIO) {
		t.Fatalf("incomplete link proof accepted: %v", err)
	}
	if len(kernel.writes) != 0 {
		t.Fatal("incomplete link proof authorized route mutation")
	}
}

func TestRoutesNativeHelperDownStateDoesNotPreventQuiesce(t *testing.T) {
	for _, flags := range []uint32{unix.RTNH_F_DEAD, unix.RTNH_F_LINKDOWN, unix.RTNH_F_DEAD | unix.RTNH_F_LINKDOWN} {
		s, kernel, _ := routeTestSystem(t)
		kernel.extraRules = []wireMessage{nativeOIFTestRule(2022, 999, 0, "utun")}
		kernel.extraRoutes = []wireMessage{nativeTUNTestRoute(2022, 10)}
		if err := s.EnsureRoutes(context.Background(), nativeHelperPolicies()); err != nil {
			t.Fatal(err)
		}
		kernel.links[1].Up = false
		wireOrder.PutUint32(kernel.extraRoutes[0].Data[8:12], flags)
		if err := s.QuiesceTUN(context.Background()); err != nil {
			t.Fatalf("TUN health flags %#x stranded the close fence: %v", flags, err)
		}
		for _, route := range kernel.routes {
			if route.Table == 101 && route.Type != unix.RTN_BLACKHOLE {
				t.Fatal("down TUN retained managed forwarding after quiesce")
			}
		}
		if err := s.CloseRoutes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(kernel.routes) != 0 || len(kernel.rules) != 0 || len(kernel.extraRules) != 1 || len(kernel.extraRoutes) != 1 {
			t.Fatal("down TUN cleanup changed native resources or left managed resources")
		}
	}
}
