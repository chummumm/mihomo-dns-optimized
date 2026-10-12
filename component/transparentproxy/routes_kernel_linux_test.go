//go:build linux

package transparentproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/sagernet/netlink"
	"golang.org/x/sys/unix"
)

// This test observes actual packets emitted by the Linux policy-routing engine.
// It never changes the caller's namespace; an explicitly requested integration
// run must fail, rather than silently skip, when isolation or privileges fail.
func TestRoutesKernelBlackholeBarrier(t *testing.T) {
	if os.Getenv("MIHOMO_TRANSPARENT_PROXY_INTEGRATION") != "1" {
		t.Skip("requires explicit privileged integration in an isolated network namespace")
	}
	currentNamespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	parentNamespace := os.Getenv("MIHOMO_ROUTES_PARENT_NAMESPACE")
	if parentNamespace == "" {
		command := exec.Command("unshare", "--net", os.Args[0], "-test.run", "^TestRoutesKernelBlackholeBarrier$", "-test.v")
		command.Env = append(os.Environ(), "MIHOMO_ROUTES_PARENT_NAMESPACE="+currentNamespace)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated route integration: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	if currentNamespace == parentNamespace {
		t.Fatal("refusing route integration in the parent namespace")
	}
	ctx := context.Background()
	instance, err := newRouteSystem()
	if err != nil {
		t.Fatal(err)
	}
	system := instance.(*linuxRouteSystem)
	defer func() {
		if err := system.CloseRoutes(ctx); err != nil {
			t.Errorf("owned route cleanup: %v", err)
		}
		if err := system.Close(); err != nil {
			t.Errorf("route transport cleanup: %v", err)
		}
	}()
	for _, path := range []string{"net/ipv6/conf/all/disable_ipv6", "net/ipv6/conf/default/disable_ipv6"} {
		if err := system.writeSysctl(path, "0"); err != nil {
			t.Fatalf("isolated IPv6 setup: %v", err)
		}
	}
	loopback, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(loopback); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "wan0"}, PeerName: "wan-peer"}); err != nil {
		t.Fatal(err)
	}
	wan, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := netlink.LinkByName("wan-peer")
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []netlink.Link{wan, peer} {
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	addKernelAddress(t, wan, "198.19.0.1/24")
	addKernelAddress(t, wan, "2001:db8:1::1/64")
	for _, gateway := range []string{"198.19.0.2", "2001:db8:1::2"} {
		address := net.ParseIP(gateway)
		family := unix.AF_INET6
		if address.To4() != nil {
			family = unix.AF_INET
			address = address.To4()
		}
		if err := netlink.NeighAdd(&netlink.Neigh{LinkIndex: wan.Attrs().Index, Family: family, State: unix.NUD_PERMANENT, IP: address, HardwareAddr: peer.Attrs().HardwareAddr}); err != nil {
			t.Fatal(err)
		}
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: wan.Attrs().Index, Family: family, Gw: address, Table: unix.RT_TABLE_MAIN}); err != nil {
			t.Fatal(err)
		}
	}
	protocol := uint16(wireOrder.Uint16([]byte{0, unix.ETH_P_ALL}))
	packetFD, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, int(protocol))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(packetFD)
	if err := unix.Bind(packetFD, &unix.SockaddrLinklayer{Protocol: protocol, Ifindex: wan.Attrs().Index}); err != nil {
		t.Fatal(err)
	}

	tunFD, tunIndex := openKernelTestTUN(t)
	defer func() {
		if tunFD >= 0 {
			_ = unix.Close(tunFD)
		}
	}()
	policies := testTUNPolicies()
	for i := range policies {
		policies[i].IfIndex = tunIndex
	}
	if err := system.EnsureRoutes(ctx, policies); err != nil {
		t.Fatal(err)
	}
	probe := func(stage string, wantTUN bool) {
		t.Helper()
		for _, ipv6 := range []bool{false, true} {
			family := "v4"
			if ipv6 {
				family = "v6"
			}
			// Every blocked probe has a successful unmarked control under the
			// same routes, neighbor entries and capture socket. A dead WAN or
			// capture failure therefore cannot masquerade as leak protection.
			control := []byte("mihomo-route-control/" + stage + "/" + family)
			if err := sendKernelProbe(ipv6, 0, control); err != nil {
				t.Fatalf("%s control send: %v", stage, err)
			}
			atWAN, _ := observeKernelProbe(t, packetFD, tunFD, control)
			if !atWAN {
				t.Fatalf("%s/%s unmarked control did not reach the WAN capture", stage, family)
			}
			payload := []byte("mihomo-route-marked/" + stage + "/" + family)
			err := sendKernelProbe(ipv6, policies[0].Mark, payload)
			if wantTUN && err != nil {
				t.Fatalf("%s/%s marked send: %v", stage, family, err)
			}
			if !wantTUN && err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENETUNREACH) && !errors.Is(err, unix.EHOSTUNREACH) && !errors.Is(err, unix.EACCES) {
				t.Fatalf("%s/%s unrelated marked send failure: %v", stage, family, err)
			}
			atWAN, atTUN := observeKernelProbe(t, packetFD, tunFD, payload)
			if atWAN {
				t.Fatalf("%s/%s marked packet escaped through the WAN", stage, family)
			}
			if atTUN != wantTUN {
				t.Fatalf("%s/%s TUN observation=%v, want %v", stage, family, atTUN, wantTUN)
			}
		}
	}
	probe("active", true)
	// Losing just the lookup must terminate at the adjacent guard, even though
	// an otherwise usable TUN and a working unmarked default route both exist.
	for _, policy := range policies {
		_, lookup := policyRules(policy)
		if _, err := system.request.Execute(ctx, unix.RTM_DELRULE, unix.NLM_F_ACK, marshalOwnedRule(lookup)); err != nil {
			t.Fatal(err)
		}
	}
	probe("lookup-deleted", false)
	if err := system.EnsureRoutes(ctx, policies); err != nil {
		t.Fatal(err)
	}
	probe("lookup-restored", true)
	if err := system.QuiesceTUN(ctx); err != nil {
		t.Fatal(err)
	}
	probe("quiesced-before-close", false)
	if err := unix.Close(tunFD); err != nil {
		t.Fatal(err)
	}
	tunFD = -1
	probe("closed-after-quiesce", false)
	// A replacement core has no file, inherited descriptor or old Go state.
	// It must discover these exact working blackholes and rules from the same
	// YAML, keep packets blocked, then attach the new TUN's actual index.
	if err := system.Close(); err != nil {
		t.Fatal(err)
	}
	instance, err = newRouteSystem()
	if err != nil {
		t.Fatal(err)
	}
	system = instance.(*linuxRouteSystem)
	for i := range policies {
		policies[i].Ready, policies[i].IfIndex = false, 0
	}
	if err := system.RecoverRoutes(ctx, policies, nil); err != nil {
		t.Fatalf("recover actual kernel protection without a journal: %v", err)
	}
	if err := system.EnsureRoutes(ctx, policies); err != nil {
		t.Fatal(err)
	}
	probe("fresh-process-keeps-blackholes", false)
	oldIndex := tunIndex
	tunFD, tunIndex = openKernelTestTUN(t)
	if tunIndex == oldIndex {
		t.Fatal("replacement TUN did not get a new test interface index")
	}
	for i := range policies {
		policies[i].IfIndex, policies[i].Ready = tunIndex, true
	}
	if err := system.EnsureRoutes(ctx, policies); err != nil {
		t.Fatal(err)
	}
	probe("replacement-ready", true)
	// Non-persistent TUN destruction bypasses the intentional-close fence.
	// Its default blackhole still exists because it has no TUN next hop.
	if err := unix.Close(tunFD); err != nil {
		t.Fatal(err)
	}
	tunFD = -1
	probe("unexpected-device-deletion", false)
	actual, err := system.routeSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies {
		policy.Ready = false
		if err := system.verifyPolicyRoutes(actual, policy, 0); err != nil {
			t.Fatalf("unexpected-delete protection: %v", err)
		}
	}
}

func addKernelAddress(t *testing.T, link netlink.Link, cidr string) {
	t.Helper()
	address, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatal(err)
	}
	address.Flags = unix.IFA_F_NODAD
	if err := netlink.AddrAdd(link, address); err != nil {
		t.Fatal(err)
	}
}

func openKernelTestTUN(t *testing.T) (int, int) {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	request, err := unix.NewIfreq("utun")
	if err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	request.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, request); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	link, err := netlink.LinkByName("utun")
	if err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	addKernelAddress(t, link, "198.18.0.1/30")
	addKernelAddress(t, link, "2001:db8:f::1/126")
	if err := netlink.LinkSetUp(link); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	return fd, link.Attrs().Index
}

func sendKernelProbe(ipv6 bool, mark uint32, payload []byte) error {
	family := unix.AF_INET
	var destination unix.Sockaddr = &unix.SockaddrInet4{Port: 40527, Addr: [4]byte{203, 0, 113, 9}}
	if ipv6 {
		family = unix.AF_INET6
		var address [16]byte
		copy(address[:], net.ParseIP("2001:db8:2::9").To16())
		destination = &unix.SockaddrInet6{Port: 40527, Addr: address}
	}
	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK, int(mark)); err != nil {
		return err
	}
	return unix.Sendto(fd, payload, 0, destination)
}

func observeKernelProbe(t *testing.T, packetFD, tunFD int, payload []byte) (bool, bool) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	seenWAN, seenTUN := false, false
	buffer := make([]byte, 65536)
	for time.Now().Before(deadline) {
		poll := []unix.PollFd{{Fd: int32(packetFD), Events: unix.POLLIN}}
		if tunFD >= 0 {
			poll = append(poll, unix.PollFd{Fd: int32(tunFD), Events: unix.POLLIN})
		}
		if _, err := unix.Poll(poll, 20); err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatal(err)
		}
		for i, descriptor := range poll {
			if descriptor.Revents&(unix.POLLNVAL|unix.POLLERR|unix.POLLHUP) != 0 {
				t.Fatal(fmt.Errorf("packet observer failed: %#x", descriptor.Revents))
			}
			if descriptor.Revents&unix.POLLIN == 0 {
				continue
			}
			for {
				n, err := unix.Read(int(descriptor.Fd), buffer)
				if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
				if bytes.Contains(buffer[:n], payload) {
					if i == 0 {
						seenWAN = true
					} else {
						seenTUN = true
					}
				}
			}
		}
	}
	return seenWAN, seenTUN
}
