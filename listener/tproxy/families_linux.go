//go:build linux

package tproxy

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func transparentFamilies(addr net.Addr, raw syscall.RawConn) (ipv4, ipv6 bool) {
	var ip net.IP
	switch addr := addr.(type) {
	case *net.TCPAddr:
		ip = addr.IP
	case *net.UDPAddr:
		ip = addr.IP
	}
	if !ip.IsUnspecified() {
		return false, false
	}
	if ip.To4() != nil {
		return true, false
	}
	var onlyIPv6 int
	var socketErr error
	err := raw.Control(func(fd uintptr) {
		onlyIPv6, socketErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
	})
	if err != nil || socketErr != nil {
		return false, false
	}
	return onlyIPv6 == 0, true
}
