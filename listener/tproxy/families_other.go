//go:build !linux

package tproxy

import (
	"net"
	"syscall"
)

func transparentFamilies(net.Addr, syscall.RawConn) (bool, bool) { return false, false }
