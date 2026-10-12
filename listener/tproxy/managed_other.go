//go:build !linux

package tproxy

import (
	"fmt"
	"net"
)

func listenManagedTCP(net.ListenConfig, string) (net.Listener, error) {
	return nil, fmt.Errorf("managed TProxy is only supported on Linux")
}

func listenManagedUDP(string) (net.PacketConn, error) {
	return nil, fmt.Errorf("managed TProxy is only supported on Linux")
}
