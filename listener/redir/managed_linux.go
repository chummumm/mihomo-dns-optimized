//go:build linux

package redir

import (
	"context"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func listenManaged(addr string) (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		err := raw.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0)
		})
		return errors.Join(err, socketErr)
	}}
	// Re-probe the kernel rather than relying on net's cached IPv6 capability
	// after the stack has been disabled and later enabled again.
	listener, err6 := config.Listen(context.Background(), "tcp6", addr)
	if err6 == nil {
		return listener, nil
	}
	listener, err4 := net.Listen("tcp4", addr)
	if err4 != nil {
		return nil, errors.Join(err6, err4)
	}
	return listener, nil
}

func listenerFamilies(listener net.Listener) (bool, bool) {
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return false, false
	}
	address, ok := tcp.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsUnspecified() {
		return false, false
	}
	if address.IP.To4() != nil {
		return true, false
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return false, false
	}
	var ipv6Only int
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		ipv6Only, socketErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
	})
	if err != nil || socketErr != nil {
		return false, false
	}
	return ipv6Only == 0, true
}
