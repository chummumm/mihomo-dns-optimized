package dialer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/transparentproxy"
)

func isolateManagedSocketPolicy(t *testing.T) {
	t.Helper()
	oldEnabled, oldChoose, oldSet := managedCaptureEnabled, managedOutboundMark, setManagedSocketMark
	oldMark, oldInterface := DefaultRoutingMark.Load(), DefaultInterface.Load()
	oldFinder, oldHook := DefaultInterfaceFinder.Load(), DefaultSocketHook
	t.Cleanup(func() {
		managedCaptureEnabled, managedOutboundMark, setManagedSocketMark = oldEnabled, oldChoose, oldSet
		DefaultRoutingMark.Store(oldMark)
		DefaultInterface.Store(oldInterface)
		DefaultInterfaceFinder.Store(oldFinder)
		DefaultSocketHook = oldHook
	})
	DefaultRoutingMark.Store(0)
	DefaultInterface.Store("")
	DefaultInterfaceFinder.Store(nil)
	DefaultSocketHook = nil
	managedCaptureEnabled = func() bool { return true }
	managedOutboundMark = func(mark uint32) (uint32, error) {
		if mark == 0 {
			return transparentproxy.DefaultOutboundMark, nil
		}
		return mark, nil
	}
	setManagedSocketMark = func(syscall.RawConn, int) error { return nil }
}

func TestManagedSocketPolicyChecksActivationAtControlTime(t *testing.T) {
	isolateManagedSocketPolicy(t)
	enabled := false
	managedCaptureEnabled = func() bool { return enabled }
	var marks []uint32
	setManagedSocketMark = func(_ syscall.RawConn, mark int) error { marks = append(marks, uint32(mark)); return nil }
	control := managedOutboundControl(0)
	enabled = true
	if err := control(context.Background(), "tcp4", "127.0.0.1:80", nil); err != nil {
		t.Fatal(err)
	}
	enabled = false
	if err := control(context.Background(), "tcp4", "127.0.0.1:80", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(marks, []uint32{transparentproxy.DefaultOutboundMark}) {
		t.Fatalf("control cached old activation or changed sockets while disabled: %v", marks)
	}
}

func TestManagedSocketPolicyPreservesFullGlobalAndAdapterMarks(t *testing.T) {
	isolateManagedSocketPolicy(t)
	for _, test := range []struct {
		name                  string
		global, adapter, want uint32
	}{
		{"default", 0, 0, transparentproxy.DefaultOutboundMark},
		{"global", 0x81234567, 0, 0x81234567},
		{"adapter overrides global", 0x100, 0xabcdef12, 0xabcdef12},
	} {
		t.Run(test.name, func(t *testing.T) {
			DefaultRoutingMark.Store(int32(test.global))
			var chosen, written []uint32
			managedOutboundMark = func(mark uint32) (uint32, error) {
				chosen = append(chosen, mark)
				if mark == 0 {
					return transparentproxy.DefaultOutboundMark, nil
				}
				return mark, nil
			}
			setManagedSocketMark = func(_ syscall.RawConn, mark int) error {
				written = append(written, uint32(mark))
				return nil
			}
			options := []Option{WithRoutingMark(int(test.adapter))}
			packet, err := ListenPacket(context.Background(), "udp4", "127.0.0.1:0", netip.AddrPort{}, options...)
			if err != nil {
				t.Fatal(err)
			}
			packet.Close()
			if err := ICMPControlWithOptions(netip.MustParseAddr("127.0.0.1"), options...)("ip4:icmp", "127.0.0.1:0", nil); err != nil {
				t.Fatal(err)
			}
			if err := SocketControlWithOptions(netip.MustParseAddr("127.0.0.1"), options...)("ip4:tcp", "127.0.0.1:0", nil); err != nil {
				t.Fatal(err)
			}
			if len(chosen) != 3 || !reflect.DeepEqual(written, []uint32{test.want, test.want, test.want}) {
				t.Fatalf("UDP/ICMP/raw socket lost the complete selected mark: registered=%#v written=%#v", chosen, written)
			}
			if uint32(DefaultRoutingMark.Load()) != test.global {
				t.Fatal("internal fallback mutated global routing-mark")
			}
		})
	}
}

func TestManagedTCPControlProtectsBeforeConnectAndRetainsExistingControl(t *testing.T) {
	isolateManagedSocketPolicy(t)
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var order []string
	setManagedSocketMark = func(_ syscall.RawConn, mark int) error {
		if uint32(mark) != transparentproxy.DefaultOutboundMark {
			t.Fatalf("unexpected fallback mark %#x", mark)
		}
		order = append(order, "protect")
		return nil
	}
	native := &net.Dialer{Control: func(string, string, syscall.RawConn) error { order = append(order, "existing"); return nil }}
	conn, err := DialContext(context.Background(), "tcp4", server.Addr().String(), WithNetDialer(native))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !reflect.DeepEqual(order, []string{"existing", "protect"}) {
		t.Fatalf("connect did not apply ordered controls: %v", order)
	}
	if native.ControlContext != nil {
		t.Fatal("dialing mutated a caller-owned net.Dialer")
	}

	rejected := errors.New("could not commit nft bypass mark")
	managedOutboundMark = func(uint32) (uint32, error) { return 0, rejected }
	setManagedSocketMark = func(syscall.RawConn, int) error { t.Fatal("failed registration reached SO_MARK"); return nil }
	blocked, err := DialContext(context.Background(), "tcp4", server.Addr().String())
	if blocked != nil || !errors.Is(err, rejected) {
		t.Fatalf("unsafe connect proceeded after protection failure: %v", err)
	}

	accepted, err := server.Accept()
	if err != nil {
		t.Fatal(err)
	}
	accepted.Close()
	server.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
	if unexpected, err := server.Accept(); err == nil {
		unexpected.Close()
		t.Fatal("failed protection still established a TCP connection")
	}
}

func TestManagedProtectionComposesWithSocketHookAndFailsClosed(t *testing.T) {
	isolateManagedSocketPolicy(t)
	DefaultRoutingMark.Store(9876)
	var order []string
	DefaultSocketHook = func(string, string, syscall.RawConn) error { order = append(order, "hook"); return nil }
	rejected := errors.New("SO_MARK denied")
	setManagedSocketMark = func(_ syscall.RawConn, mark int) error {
		if mark != 9876 {
			t.Fatalf("socket hook discarded explicit global mark: %d", mark)
		}
		order = append(order, "protect")
		return rejected
	}
	if err := ICMPControl(netip.MustParseAddr("127.0.0.1"))("ip4:icmp", "127.0.0.1:0", nil); !errors.Is(err, rejected) {
		t.Fatalf("socket mark failure was ignored: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"hook", "protect"}) {
		t.Fatalf("socket hook bypassed managed protection: %v", order)
	}
}
