package tproxy

import (
	"net"
	"net/netip"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/common/pool"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/transport/socks5"

	"golang.org/x/exp/slices"
)

type UDPListener struct {
	packetConn net.PacketConn
	addr       string
	closed     bool
	ipv4       bool
	ipv6       bool
}

func (l *UDPListener) TransparentFamilies() (bool, bool) { return l.ipv4, l.ipv6 }

// RawAddress implements C.Listener
func (l *UDPListener) RawAddress() string {
	return l.addr
}

// Address implements C.Listener
func (l *UDPListener) Address() string {
	return l.packetConn.LocalAddr().String()
}

// Close implements C.Listener
func (l *UDPListener) Close() error {
	l.closed = true
	return l.packetConn.Close()
}

func NewUDP(addr string, tunnel C.Tunnel, additions ...inbound.Addition) (*UDPListener, error) {
	return newUDPListener(addr, tunnel, false, additions...)
}

func NewManagedUDP(addr string, tunnel C.Tunnel) (*UDPListener, error) {
	return newUDPListener(addr, tunnel, true)
}

func newUDPListener(addr string, tunnel C.Tunnel, managed bool, additions ...inbound.Addition) (*UDPListener, error) {
	if len(additions) == 0 {
		additions = []inbound.Addition{
			inbound.WithInName("DEFAULT-TPROXY"),
			inbound.WithSpecialRules(""),
		}
	}
	var l net.PacketConn
	var err error
	if managed {
		l, err = listenManagedUDP(addr)
	} else {
		l, err = net.ListenPacket("udp", addr)
	}
	if err != nil {
		return nil, err
	}

	rl := &UDPListener{
		packetConn: l,
		addr:       addr,
	}

	c := l.(*net.UDPConn)

	rc, err := c.SyscallConn()
	if err != nil {
		l.Close()
		return nil, err
	}

	err = setsockopt(rc, addr)
	if err != nil {
		l.Close()
		return nil, err
	}
	rl.ipv4, rl.ipv6 = transparentFamilies(l.LocalAddr(), rc)

	go func() {
		oob := make([]byte, 1024)
		for {
			buf := pool.Get(pool.UDPBufferSize)
			n, oobn, _, lAddr, err := c.ReadMsgUDPAddrPort(buf, oob)
			if err != nil {
				pool.Put(buf)
				if rl.closed {
					break
				}
				continue
			}

			rAddr, err := getOrigDst(oob[:oobn])
			if err != nil {
				pool.Put(buf)
				continue
			}

			dscp, _ := getDSCP(oob[:oobn])
			additions := append(slices.Clip(additions), inbound.WithDSCP(dscp)) // don't change outside additions

			if rAddr.Addr().Is4() {
				// try to unmap 4in6 address
				lAddr = netip.AddrPortFrom(lAddr.Addr().Unmap(), lAddr.Port())
			}
			handlePacketConn(l, tunnel, buf[:n], lAddr, rAddr, additions...)
		}
	}()

	return rl, nil
}

func handlePacketConn(pc net.PacketConn, tunnel C.Tunnel, buf []byte, lAddr, rAddr netip.AddrPort, additions ...inbound.Addition) {
	target := socks5.AddrFromStdAddrPort(rAddr)
	pkt := &packet{
		pc:        pc,
		lAddr:     lAddr,
		buf:       buf,
		tunnel:    tunnel,
		additions: additions,
	}
	tunnel.HandleUDPPacket(inbound.NewPacket(target, pkt, C.TPROXY, additions...))
}
