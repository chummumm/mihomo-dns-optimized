//go:build linux

package tproxy

import (
	"context"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Explicit IPv6 sockets re-probe the kernel after a disabled IPv6 stack is
// restored. A generic "tcp"/"udp" listen can keep net's startup capability
// cache and remain IPv4-only for the lifetime of this process.
func managedDualStackControl(_, _ string, raw syscall.RawConn) error {
	var socketErr error
	err := raw.Control(func(fd uintptr) {
		socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0)
	})
	return errors.Join(err, socketErr)
}

func listenManagedTCP(lc net.ListenConfig, addr string) (net.Listener, error) {
	dual := lc
	dual.Control = managedDualStackControl
	l, err6 := dual.Listen(context.Background(), "tcp6", addr)
	if err6 == nil {
		return l, nil
	}
	l, err4 := lc.Listen(context.Background(), "tcp4", addr)
	if err4 != nil {
		return nil, errors.Join(err6, err4)
	}
	return l, nil
}

func listenManagedUDP(addr string) (net.PacketConn, error) {
	dual := net.ListenConfig{Control: managedDualStackControl}
	l, err6 := dual.ListenPacket(context.Background(), "udp6", addr)
	if err6 == nil {
		return l, nil
	}
	l, err4 := net.ListenPacket("udp4", addr)
	if err4 != nil {
		return nil, errors.Join(err6, err4)
	}
	return l, nil
}
