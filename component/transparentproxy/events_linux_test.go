//go:build linux

package transparentproxy

import (
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

func eventTestAddress(message uint16, prefix string, valid, preferred, flags uint32) wirePacket {
	address := netip.MustParsePrefix(prefix)
	data := make([]byte, 8)
	data[0], data[1] = unix.AF_INET6, byte(address.Bits())
	wireOrder.PutUint32(data[4:8], 4)
	data = appendWireAttribute(data, unix.IFA_ADDRESS, address.Addr().AsSlice())
	data = appendWireAttribute(data, unix.IFA_FLAGS, wireUint32(flags))
	lifetime := make([]byte, 16)
	wireOrder.PutUint32(lifetime[0:4], preferred)
	wireOrder.PutUint32(lifetime[4:8], valid)
	data = appendWireAttribute(data, unix.IFA_CACHEINFO, lifetime)
	return wirePacket{wireMessage: wireMessage{Type: message, Data: data}}
}

func eventTestRoute(message uint16, prefix string, table, expires, metric uint32) wirePacket {
	destination := netip.MustParsePrefix(prefix)
	data := make([]byte, 12)
	data[0] = unix.AF_INET6
	if destination.Addr().Is4() {
		data[0] = unix.AF_INET
	}
	data[1], data[5], data[7] = byte(destination.Bits()), unix.RTPROT_RA, unix.RTN_UNICAST
	data = appendWireAttribute(data, unix.RTA_TABLE, wireUint32(table))
	data = appendWireAttribute(data, unix.RTA_DST, destination.Masked().Addr().AsSlice())
	data = appendWireAttribute(data, unix.RTA_OIF, wireUint32(2))
	data = appendWireAttribute(data, unix.RTA_PRIORITY, wireUint32(metric))
	data = appendWireAttribute(data, unix.RTA_EXPIRES, wireUint32(expires))
	return wirePacket{wireMessage: wireMessage{Type: message, Data: data}}
}

func assertEventNotification(t *testing.T, filter *systemNotificationFilter, packet wirePacket, nftables, wantChanged, wantUrgent bool) {
	t.Helper()
	changed, urgent, err := filter.notification(packet, nftables)
	if err != nil || changed != wantChanged || urgent != wantUrgent {
		t.Fatalf("notification %d: changed=%v urgent=%v err=%v; want %v/%v", packet.Type, changed, urgent, err, wantChanged, wantUrgent)
	}
}

func TestAddressNotificationsIgnoreRenewalButRetainUsabilityTransitions(t *testing.T) {
	var filter systemNotificationFilter
	const prefix = "240e:1234:5678:abcd::12/64"
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 3600, 1800, 0), false, true, false)
	for i := uint32(1); i <= 64; i++ {
		assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 3600+i, 1800-i, 0), false, false, false)
	}
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 7200, 0, unix.IFA_F_DEPRECATED), false, false, false)
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 0, 0, 0), false, true, true)
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 3600, 1800, unix.IFA_F_TENTATIVE), false, false, false)
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 3600, 1800, 0), false, true, false)
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 3600, 1800, unix.IFA_F_DADFAILED), false, true, true)
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_DELADDR, prefix, 0, 0, 0), false, true, true)
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 3600, 1800, 0), false, true, false)
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, "240e:1234:5678:dcba::12/64", 3600, 1800, 0), false, true, false)

	// A resubscription never reuses assumptions from the lost stream.
	filter = systemNotificationFilter{}
	assertEventNotification(t, &filter, eventTestAddress(unix.RTM_NEWADDR, prefix, 3600, 1800, 0), false, true, false)
}

func TestRouteNotificationsExcludeUnrelatedRIOButRetainProbeOverridesAndManagedTables(t *testing.T) {
	for _, test := range []struct {
		name    string
		prefix  string
		table   uint32
		changed bool
		urgent  bool
	}{
		{"thread-ula", "fd12:3456:789a::/64", unix.RT_TABLE_MAIN, false, false},
		{"unrelated-public-rio", "240e:1234:5678::/48", unix.RT_TABLE_MAIN, false, false},
		{"unrelated-ipv4", "10.0.0.0/8", unix.RT_TABLE_MAIN, false, false},
		{"ipv6-default", "::/0", unix.RT_TABLE_MAIN, true, false},
		{"ipv4-default", "0.0.0.0/0", unix.RT_TABLE_MAIN, true, false},
		{"ipv6-probe-override", "2606:4700:4700::/64", unix.RT_TABLE_MAIN, true, false},
		{"ipv4-probe-override", "1.1.1.0/24", unix.RT_TABLE_MAIN, true, false},
		{"managed-table", "fd12:3456:789a::/64", 101, true, true},
		{"native-tun-table", "fd12:3456:789a::/64", 2022, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var filter systemNotificationFilter
			assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, test.prefix, test.table, 3600, 1024), false, test.changed, test.urgent)
			assertEventNotification(t, &filter, eventTestRoute(unix.RTM_DELROUTE, test.prefix, test.table, 0, 1024), false, test.changed, test.changed)
		})
	}
}

func TestRADefaultRenewalsKeepChangesAndReturnsToEarlierRoutes(t *testing.T) {
	var filter systemNotificationFilter
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, 1800, 1024), false, true, false)
	for expires := uint32(1801); expires < 1865; expires++ {
		assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, expires, 1024), false, false, false)
	}
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, 1800, 2048), false, true, false)
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, 1800, 1024), false, true, false)
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, 0, 1024), false, true, false)
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_DELROUTE, "::/0", unix.RT_TABLE_MAIN, 0, 1024), false, true, true)
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, 1800, 1024), false, true, false)
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "2606:4700:4700::/64", unix.RT_TABLE_MAIN, 1800, 1024), false, true, false)
	assertEventNotification(t, &filter, eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, 1800, 1024), false, true, false)
}

func TestRAKernelCacheInfoRenewalAndPreferenceChanges(t *testing.T) {
	var filter systemNotificationFilter
	route := func(expires int32, uses uint32, gateway string, preference byte) wirePacket {
		packet := eventTestRoute(unix.RTM_NEWROUTE, "::/0", unix.RT_TABLE_MAIN, 1800, 1024)
		attributes, _ := parseWireAttributes(packet.Data[12:])
		data := append([]byte(nil), packet.Data[:12]...)
		for _, attribute := range attributes {
			if attribute.Type&wireAttributeTypeMask != unix.RTA_EXPIRES {
				data = appendWireAttribute(data, attribute.Type, attribute.Data)
			}
		}
		cache := make([]byte, 32)
		wireOrder.PutUint32(cache[8:12], uint32(expires))
		wireOrder.PutUint32(cache[16:20], uses)
		data = appendWireAttribute(data, unix.RTA_CACHEINFO, cache)
		data = appendWireAttribute(data, unix.RTA_GATEWAY, netip.MustParseAddr(gateway).AsSlice())
		data = appendWireAttribute(data, unix.RTA_PREF, []byte{preference})
		packet.Data = data
		return packet
	}
	assertEventNotification(t, &filter, route(180000, 0, "fe80::1", 0), false, true, false)
	assertEventNotification(t, &filter, route(179900, 10, "fe80::1", 0), false, false, false)
	assertEventNotification(t, &filter, route(360000, 20, "fe80::1", 0), false, false, false)
	assertEventNotification(t, &filter, route(360000, 20, "fe80::2", 0), false, true, false)
	assertEventNotification(t, &filter, route(360000, 20, "fe80::1", 0), false, true, false)
	assertEventNotification(t, &filter, route(360000, 20, "fe80::1", 1), false, true, false)
	assertEventNotification(t, &filter, route(0, 20, "fe80::1", 1), false, true, false)
	assertEventNotification(t, &filter, route(-1, 20, "fe80::1", 1), false, true, false)
}

func TestLinkNotificationsKeepCarrierAndDeviceChanges(t *testing.T) {
	var filter systemNotificationFilter
	link := func(message uint16, flags, mtu uint32, name string) wirePacket {
		data := make([]byte, 16)
		wireOrder.PutUint32(data[4:8], 4)
		wireOrder.PutUint32(data[8:12], flags)
		data = appendWireAttribute(data, unix.IFLA_IFNAME, append([]byte(name), 0))
		data = appendWireAttribute(data, unix.IFLA_MTU, wireUint32(mtu))
		return wirePacket{wireMessage: wireMessage{Type: message, Data: data}}
	}
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, unix.IFF_UP|unix.IFF_RUNNING, 1500, "utun"), false, true, true)
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, unix.IFF_UP|unix.IFF_RUNNING, 1500, "utun"), false, false, false)
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, unix.IFF_UP, 1500, "utun"), false, true, true)
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, unix.IFF_UP, 9000, "utun"), false, true, true)
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, 0, 1500, "utun"), false, true, true)
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, unix.IFF_UP, 1500, "utun"), false, true, true)
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, unix.IFF_UP, 1500, "tun1"), false, true, true)
	assertEventNotification(t, &filter, link(unix.RTM_DELLINK, unix.IFF_UP, 1500, "tun1"), false, true, true)
	assertEventNotification(t, &filter, link(unix.RTM_NEWLINK, unix.IFF_UP, 1500, "tun1"), false, true, true)
	packet := link(unix.RTM_NEWLINK, unix.IFF_UP, 1500, "tun1")
	packet.Data = appendWireAttribute(packet.Data, unix.IFLA_STATS64, make([]byte, 200))
	assertEventNotification(t, &filter, packet, false, false, false)
	packet.Data = appendWireAttribute(packet.Data, unix.IFLA_CARRIER, []byte{0})
	assertEventNotification(t, &filter, packet, false, true, true)
}

func TestRuleAndNFTChangesAlwaysInvalidateImmediately(t *testing.T) {
	var filter systemNotificationFilter
	for _, message := range []uint16{unix.RTM_NEWRULE, unix.RTM_DELRULE} {
		data := make([]byte, 12)
		data[0], data[7] = unix.AF_INET6, unix.FR_ACT_TO_TBL
		packet := wirePacket{wireMessage: wireMessage{Type: message, Data: data}}
		assertEventNotification(t, &filter, packet, false, true, true)
		assertEventNotification(t, &filter, packet, false, true, true)
	}
	data := appendWireAttribute(make([]byte, 4), unix.NFTA_GEN_ID, []byte{0, 0, 0, 7})
	packet := wirePacket{wireMessage: wireMessage{Type: unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_NEWGEN, Data: data}}
	assertEventNotification(t, &filter, packet, true, true, true)
	assertEventNotification(t, &filter, packet, true, true, true)
	packet.Type = unix.NFNL_SUBSYS_NFTABLES<<8 | unix.NFT_MSG_NEWRULE
	assertEventNotification(t, &filter, packet, true, false, false)
}

func TestNotificationFilteringDoesNotHideMalformedOrLostInput(t *testing.T) {
	for _, packet := range []wirePacket{
		{wireMessage: wireMessage{Type: unix.NLMSG_OVERRUN}},
		{wireMessage: wireMessage{Type: unix.RTM_NEWADDR, Data: make([]byte, 7)}},
		{wireMessage: wireMessage{Type: unix.RTM_NEWROUTE, Data: make([]byte, 11)}},
	} {
		var filter systemNotificationFilter
		if changed, _, err := filter.notification(packet, false); err == nil || changed {
			t.Fatalf("accepted incomplete stream: %+v, %v", packet, err)
		}
	}
}
