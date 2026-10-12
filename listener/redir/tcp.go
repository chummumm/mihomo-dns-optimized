package redir

import (
	"net"
	"sync/atomic"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/component/keepalive"
	C "github.com/metacubex/mihomo/constant"
)

type Listener struct {
	listener net.Listener
	addr     string
	closed   atomic.Bool
}

// RawAddress implements C.Listener
func (l *Listener) RawAddress() string {
	return l.addr
}

// Address implements C.Listener
func (l *Listener) Address() string {
	return l.listener.Addr().String()
}

// Close implements C.Listener
func (l *Listener) Close() error {
	l.closed.Store(true)
	return l.listener.Close()
}

// TransparentFamilies reports actual wildcard socket coverage for managed
// REDIRECT capture. A configured port is not proof of IPv6 availability.
func (l *Listener) TransparentFamilies() (bool, bool) {
	if l.closed.Load() {
		return false, false
	}
	return listenerFamilies(l.listener)
}

func New(addr string, tunnel C.Tunnel, additions ...inbound.Addition) (*Listener, error) {
	return newListener(addr, tunnel, false, additions...)
}

func NewManaged(addr string, tunnel C.Tunnel, additions ...inbound.Addition) (*Listener, error) {
	return newListener(addr, tunnel, true, additions...)
}

func newListener(addr string, tunnel C.Tunnel, managed bool, additions ...inbound.Addition) (*Listener, error) {
	if len(additions) == 0 {
		additions = []inbound.Addition{
			inbound.WithInName("DEFAULT-REDIR"),
			inbound.WithSpecialRules(""),
		}
	}

	var l net.Listener
	var err error
	if managed {
		l, err = listenManaged(addr)
	} else {
		l, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	rl := &Listener{
		listener: l,
		addr:     addr,
	}

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				if rl.closed.Load() {
					break
				}
				continue
			}
			go handleRedir(c, tunnel, additions...)
		}
	}()

	return rl, nil
}

func handleRedir(conn net.Conn, tunnel C.Tunnel, additions ...inbound.Addition) {
	target, err := parserPacket(conn)
	if err != nil {
		conn.Close()
		return
	}
	keepalive.TCPKeepAlive(conn)
	tunnel.HandleTCPConn(inbound.NewSocket(target, conn, C.REDIR, additions...))
}
