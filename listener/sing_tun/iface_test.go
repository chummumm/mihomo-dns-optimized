package sing_tun

import (
	"net"
	"net/netip"
	"reflect"
	"testing"
)

func TestInterfacePrefixesPreserveSourcesAndCanonicalizeMasks(t *testing.T) {
	addresses := []net.Addr{
		&net.IPNet{IP: net.ParseIP("2001:db8:1::abcd"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.IPv4(192, 0, 2, 7), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("::ffff:192.0.2.7"), Mask: net.CIDRMask(120, 128)},
		&net.IPNet{IP: net.IP{192, 0, 2, 7}, Mask: net.CIDRMask(24, 32)},
		&net.IPAddr{IP: net.ParseIP("fe80::1234"), Zone: "eth0"},
		&net.IPAddr{IP: net.IPv4(198, 51, 100, 8)},
		&net.IPNet{IP: net.ParseIP("2001:db8:1::abcd"), Mask: net.CIDRMask(64, 128)},
	}
	got, err := interfacePrefixesFromAddresses(addresses)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.7/24"),
		netip.MustParsePrefix("198.51.100.8/32"),
		netip.MustParsePrefix("2001:db8:1::abcd/64"),
		netip.MustParsePrefix("fe80::1234/128"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source address or canonical representation changed: got %v, want %v", got, want)
	}
	// A genuine /0 mask differs from Mask.Size returning (0, 0) on an invalid
	// mask. A successful empty enumeration also remains a real empty snapshot.
	for _, addresses := range [][]net.Addr{
		nil,
		{&net.IPNet{IP: net.IPv4(192, 0, 2, 7), Mask: net.CIDRMask(0, 32)}},
		{&net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(0, 128)}},
	} {
		if _, err := interfacePrefixesFromAddresses(addresses); err != nil {
			t.Fatalf("valid address snapshot rejected: %v", err)
		}
	}
}

func TestInterfacePrefixesRejectIncompleteAddressSnapshots(t *testing.T) {
	tests := map[string]net.Addr{
		"non-contiguous IPv4 mask": &net.IPNet{IP: net.IPv4(192, 0, 2, 7), Mask: net.IPMask{255, 0, 255, 0}},
		"non-contiguous IPv6 mask": &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.IPMask{255, 255, 0, 255, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		"missing mask":             &net.IPNet{IP: net.IPv4(192, 0, 2, 7)},
		"wrong mask length":        &net.IPNet{IP: net.IPv4(192, 0, 2, 7), Mask: net.IPMask{255, 255}},
		"IPv4 mask on IPv6":        &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(24, 32)},
		"IPv6 mask on IPv4":        &net.IPNet{IP: net.IP{192, 0, 2, 7}, Mask: net.CIDRMask(64, 128)},
		"invalid mapped mask":      &net.IPNet{IP: net.ParseIP("::ffff:192.0.2.7"), Mask: net.CIDRMask(64, 128)},
		"missing IP":               &net.IPNet{Mask: net.CIDRMask(24, 32)},
		"invalid IP":               &net.IPAddr{IP: net.IP{192, 0, 2}},
		"nil IPNet":                (*net.IPNet)(nil),
		"nil IPAddr":               (*net.IPAddr)(nil),
		"unsupported address":      &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7)},
		"nil address":              nil,
	}
	for name, invalid := range tests {
		t.Run(name, func(t *testing.T) {
			valid := &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)}
			if got, err := interfacePrefixesFromAddresses([]net.Addr{valid, invalid}); err == nil || got != nil {
				t.Fatalf("incomplete snapshot accepted as source loss: prefixes=%v, error=%v", got, err)
			}
		})
	}
	if got, err := readDefaultInterface(net.Interface{Index: -1, Name: "invalid"}); err == nil || got != nil {
		t.Fatalf("failed interface address read became an empty interface: interface=%v, error=%v", got, err)
	}
}
