//go:build !linux

package redir

import "net"

func listenManaged(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
func listenerFamilies(net.Listener) (bool, bool)      { return false, false }
