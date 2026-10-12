//go:build linux && !386

package transparentproxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/metacubex/nftables"
	"github.com/sagernet/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Actual TCP sessions exercise conntrack's one-time NAT decision, OUTPUT
// rerouting, loopback PREROUTING, and a separate veth client's gateway path.
// Packet simulation and expression round-trips cannot prove these semantics.
func TestNFTRedirKernelTraffic(t *testing.T) {
	if os.Getenv("MIHOMO_TRANSPARENT_PROXY_INTEGRATION") != "1" {
		t.Skip("requires explicit privileged integration in an isolated network namespace")
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	parent := os.Getenv("MIHOMO_REDIR_PARENT_NAMESPACE")
	if parent == "" {
		command := exec.Command("unshare", "--net", os.Args[0], "-test.run", "^TestNFTRedirKernelTraffic$", "-test.v", "-test.timeout", "45s")
		command.Env = append(os.Environ(), "MIHOMO_REDIR_PARENT_NAMESPACE="+current)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated REDIRECT integration: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	if parent == current {
		t.Fatal("refusing REDIRECT integration in the parent namespace")
	}
	for _, name := range []string{"all", "default"} {
		if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+name+"/disable_ipv6", []byte("0"), 0); err != nil {
			t.Fatal(err)
		}
	}
	loopback, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(loopback); err != nil {
		t.Fatal(err)
	}
	addKernelAddress(t, loopback, "198.18.0.2/32")
	addKernelAddress(t, loopback, "2001:db8:42::2/128")
	client := redirKernelClientNamespace(t)
	defer client.Close()
	directPort := redirKernelStartEcho(t, "198.18.0.2", "2001:db8:42::2", 0, "direct", false)
	firstPort := redirKernelStartEcho(t, "0.0.0.0", "::", 0, "first", true)
	secondPort := redirKernelStartEcho(t, "0.0.0.0", "::", 0, "second", true)
	instance, err := newFirewall()
	if err != nil {
		t.Fatal(err)
	}
	firewall := instance.(*nftFirewall)
	ctx := context.Background()
	defer firewall.Close(ctx)
	state := nftTestState()
	state.TCP, state.UDP = ModeOff, ModeOff
	state.Local4, state.Local6, state.Gateway = []string{"*"}, []string{"*"}, []string{"gw0"}
	state.BypassUIDs, state.BypassTCPPorts, state.BypassUDPPorts = nil, nil, nil
	state.BypassMarks = []uint32{DefaultOutboundMark}
	state.FakeIP = parsePrefixes([]string{"198.18.0.2/32", "2001:db8:42::2/128"})
	state.RedirPort, state.Redir4, state.Redir6 = firstPort, true, true
	apply := func() {
		t.Helper()
		if err := firewall.Apply(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"local", "gateway"} {
		for _, address := range []string{"198.18.0.2", "2001:db8:42::2"} {
			t.Run(path+"/"+address, func(t *testing.T) {
				namespace := netns.None()
				if path == "gateway" {
					namespace = client
				}
				network := "tcp4"
				if strings.Contains(address, ":") {
					network = "tcp6"
				}
				target := net.JoinHostPort(address, strconv.Itoa(int(directPort)))
				connect := func(want string) net.Conn {
					t.Helper()
					conn, err := redirKernelDial(namespace, network, target)
					if err != nil {
						t.Fatalf("%s TCP connect: %v", want, err)
					}
					t.Cleanup(func() { conn.Close() })
					redirKernelProbe(t, conn, want, target)
					return conn
				}
				state.TCP, state.RedirPort, state.Redir4, state.Redir6 = ModeOff, firstPort, true, true
				apply()
				oldDirect := connect("direct")
				state.TCP = ModeRedir
				apply()
				redirKernelBlocked(t, oldDirect, "established direct connection skipped first-packet NAT")
				oldDirect.Close()
				active := connect("first")
				// Removing only the first-packet NAT program must leave the
				// per-packet guard able to prevent a fallback to the direct server.
				actual, err := readNFTSnapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				natChain := "redir_nat_out"
				if path == "gateway" {
					natChain = "redir_nat_pre"
				}
				admin, err := nftables.New()
				if err != nil {
					t.Fatal(err)
				}
				admin.FlushChain(actual.chains[natChain].chain)
				if err := admin.Flush(); err != nil {
					t.Fatal(err)
				}
				if conn, err := redirKernelDial(namespace, network, target); err == nil {
					conn.Close()
					t.Fatal("new session escaped after its NAT chain disappeared")
				}
				apply()
				connect("first").Close()
				state.Redir4, state.Redir6 = false, false
				apply()
				redirKernelBlocked(t, active, "unavailable REDIRECT listener left existing NAT enabled")
				active.Close()
				state.Redir4, state.Redir6 = true, true
				apply()
				oldPort := connect("first")
				state.RedirPort = secondPort
				apply()
				redirKernelBlocked(t, oldPort, "old REDIRECT port remained usable after listener replacement")
				oldPort.Close()
				currentPort := connect("second")
				if err := firewall.Close(ctx); err != nil {
					t.Fatal(err)
				}
				actual, err = readNFTSnapshot(ctx)
				if err != nil || actual.table != nil {
					t.Fatalf("normal stop left nftables resources: %v %+v", err, actual)
				}
				// Old conntrack entries are not ownership records. Once capture
				// is intentionally stopped, new sessions reach the direct server;
				// no promise is made about old NAT sessions after hook removal.
				currentPort.Close()
				connect("direct").Close()
			})
		}
	}
}

func redirKernelClientNamespace(t *testing.T) netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	client, createErr := netns.New()
	restoreErr := netns.Set(original)
	original.Close()
	runtime.UnlockOSThread()
	if err := errors.Join(createErr, restoreErr); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "gw0"}, PeerName: "client0"}); err != nil {
		t.Fatal(err)
	}
	serverLink, err := netlink.LinkByName("gw0")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := netlink.LinkByName("client0")
	if err != nil {
		t.Fatal(err)
	}
	peerMAC := peer.Attrs().HardwareAddr
	if err := netlink.LinkSetNsFd(peer, int(client)); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(serverLink); err != nil {
		t.Fatal(err)
	}
	addKernelAddress(t, serverLink, "192.0.2.1/24")
	addKernelAddress(t, serverLink, "2001:db8:41::1/64")
	handle, err := netlink.NewHandleAt(client, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	clientLink, err := handle.LinkByName("client0")
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.LinkSetUp(clientLink); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"192.0.2.2/24", "2001:db8:41::2/64"} {
		address, err := netlink.ParseAddr(prefix)
		if err != nil {
			t.Fatal(err)
		}
		address.Flags = unix.IFA_F_NODAD
		if err := handle.AddrAdd(clientLink, address); err != nil {
			t.Fatal(err)
		}
	}
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		gateway, source := net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.2")
		if family == unix.AF_INET6 {
			gateway, source = net.ParseIP("2001:db8:41::1"), net.ParseIP("2001:db8:41::2")
		}
		if err := handle.RouteAdd(&netlink.Route{Family: family, LinkIndex: clientLink.Attrs().Index, Gw: gateway}); err != nil {
			t.Fatal(err)
		}
		if err := handle.NeighAdd(&netlink.Neigh{Family: family, LinkIndex: clientLink.Attrs().Index, IP: gateway, HardwareAddr: serverLink.Attrs().HardwareAddr, State: unix.NUD_PERMANENT}); err != nil {
			t.Fatal(err)
		}
		if err := netlink.NeighAdd(&netlink.Neigh{Family: family, LinkIndex: serverLink.Attrs().Index, IP: source, HardwareAddr: peerMAC, State: unix.NUD_PERMANENT}); err != nil {
			t.Fatal(err)
		}
	}
	return client
}

func redirKernelDial(namespace netns.NsHandle, network, address string) (net.Conn, error) {
	dial := func() (net.Conn, error) { return (&net.Dialer{Timeout: 300 * time.Millisecond}).Dial(network, address) }
	if namespace == netns.None() {
		return dial()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		return nil, err
	}
	defer original.Close()
	if err := netns.Set(namespace); err != nil {
		return nil, err
	}
	conn, dialErr := dial()
	return conn, errors.Join(dialErr, netns.Set(original))
}

func redirKernelStartEcho(t *testing.T, address4, address6 string, port uint16, label string, originalDestination bool) uint16 {
	t.Helper()
	var mu sync.Mutex
	var connections []net.Conn
	for _, entry := range []struct{ network, address string }{{"tcp4", address4}, {"tcp6", address6}} {
		listener, err := net.Listen(entry.network, net.JoinHostPort(entry.address, strconv.Itoa(int(port))))
		if err != nil {
			t.Fatal(err)
		}
		port = uint16(listener.Addr().(*net.TCPAddr).Port)
		t.Cleanup(func() {
			listener.Close()
			mu.Lock()
			defer mu.Unlock()
			for _, conn := range connections {
				conn.Close()
			}
		})
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				connections = append(connections, conn)
				mu.Unlock()
				go func() {
					defer conn.Close()
					destination := conn.LocalAddr().String()
					if originalDestination {
						original, err := redirKernelOriginalDestination(conn.(*net.TCPConn))
						if err != nil {
							fmt.Fprintf(conn, "original-destination-error: %v\n", err)
							return
						}
						destination = original.String()
					}
					buffer := make([]byte, 1)
					for {
						if _, err := conn.Read(buffer); err != nil {
							return
						}
						if _, err := fmt.Fprintf(conn, "%s %s\n", label, destination); err != nil {
							return
						}
					}
				}()
			}
		}()
	}
	return port
}

func redirKernelProbe(t *testing.T, conn net.Conn, label, target string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != label+" "+target+"\n" {
		t.Fatalf("TCP destination/translation mismatch: got=%q want=%q err=%v", line, label+" "+target+"\n", err)
	}
}

func redirKernelBlocked(t *testing.T, conn net.Conn, stage string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := conn.Write([]byte{1}); err != nil {
		return
	}
	buffer := make([]byte, 128)
	if n, err := conn.Read(buffer); n != 0 || err == nil {
		t.Fatalf("%s: received %q err=%v", stage, buffer[:n], err)
	}
}

func redirKernelOriginalDestination(conn *net.TCPConn) (netip.AddrPort, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	level := unix.IPPROTO_IPV6
	ipv4 := conn.LocalAddr().(*net.TCPAddr).IP.To4() != nil
	if ipv4 {
		level = unix.IPPROTO_IP
	}
	var destination netip.AddrPort
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		var address unix.RawSockaddrAny
		length := uint32(unsafe.Sizeof(address))
		// Linux exposes SO_ORIGINAL_DST / IP6T_SO_ORIGINAL_DST at option 80.
		_, _, errno := syscall.Syscall6(unix.SYS_GETSOCKOPT, fd, uintptr(level), 80, uintptr(unsafe.Pointer(&address)), uintptr(unsafe.Pointer(&length)), 0)
		if errno != 0 {
			socketErr = errno
			return
		}
		if ipv4 {
			value := (*unix.RawSockaddrInet4)(unsafe.Pointer(&address))
			port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&value.Port))[:])
			destination = netip.AddrPortFrom(netip.AddrFrom4(value.Addr), port)
		} else {
			value := (*unix.RawSockaddrInet6)(unsafe.Pointer(&address))
			port := binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&value.Port))[:])
			destination = netip.AddrPortFrom(netip.AddrFrom16(value.Addr), port)
		}
	})
	return destination, errors.Join(err, socketErr)
}
