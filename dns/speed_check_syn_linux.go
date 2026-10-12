//go:build linux

package dns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/metacubex/mihomo/component/dialer"
	"golang.org/x/sys/unix"
)

func validateTCPSYNPlatform() error { return nil }

func probeTCPSYN(ctx context.Context, destination netip.Addr, port uint16, options ...dialer.Option) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, tcpSYNSetupError(ctx, err)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultSpeedCheckTimeout)
		defer cancel()
	}
	destination = destination.Unmap()
	if !destination.IsValid() || destination.IsUnspecified() || destination.IsMulticast() || port == 0 {
		return 0, errors.New("TCP SYN speed check requires a unicast IP and nonzero port")
	}
	source, err := tcpSYNSource(ctx, destination, port, options...)
	if err != nil {
		return 0, tcpSYNSetupError(ctx, err)
	}
	reserved, sourcePort, err := reserveTCPSYNPort(source)
	if err != nil {
		return 0, tcpSYNSetupError(ctx, fmt.Errorf("reserve TCP SYN source port: %w", err))
	}
	defer unix.Close(reserved)
	network := "ip4:tcp"
	if destination.Is6() {
		network = "ip6:tcp"
	}
	listener := net.ListenConfig{Control: dialer.SocketControlWithOptions(destination, options...)}
	packetConn, err := listener.ListenPacket(ctx, network, source.String())
	if err != nil {
		err = tcpSYNSetupError(ctx, err)
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPROTONOSUPPORT) || errors.Is(err, unix.EAFNOSUPPORT) {
			return 0, fmt.Errorf("%w: opening %s needs kernel raw TCP support and CAP_NET_RAW: %w", errTCPSYNUnavailable, network, err)
		}
		return 0, fmt.Errorf("open TCP SYN raw socket: %w", err)
	}
	defer packetConn.Close()
	stop := context.AfterFunc(ctx, func() { _ = packetConn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := packetConn.SetDeadline(deadline); err != nil {
			return 0, tcpSYNSetupError(ctx, err)
		}
	}
	var sequence [4]byte
	if _, err := rand.Read(sequence[:]); err != nil {
		return 0, tcpSYNSetupError(ctx, fmt.Errorf("generate TCP SYN sequence: %w", err))
	}
	probe := tcpSYNProbe{
		source: netip.AddrPortFrom(source, sourcePort), destination: netip.AddrPortFrom(destination, port),
		sequence: binary.BigEndian.Uint32(sequence[:]),
	}
	remote := &net.IPAddr{IP: net.IP(destination.AsSlice()), Zone: destination.Zone()}
	wire := probe.request()
	if err := ctx.Err(); err != nil {
		return 0, tcpSYNSetupError(ctx, err)
	}
	started := time.Now()
	if _, err := packetConn.WriteTo(wire, remote); err != nil {
		return 0, tcpSYNContextError(ctx, "send TCP SYN", err)
	}
	// SYN replies are small, but validate the complete received segment so a
	// forged header or truncated payload cannot accidentally pass its checksum.
	buffer := make([]byte, 65535)
	for {
		n, from, err := packetConn.ReadFrom(buffer)
		if err != nil {
			return 0, tcpSYNContextError(ctx, "receive TCP SYN reply", err)
		}
		ipAddr, ok := from.(*net.IPAddr)
		if !ok {
			continue
		}
		fromIP, ok := netip.AddrFromSlice(ipAddr.IP)
		if !ok {
			continue
		}
		remoteSequence, synAck, valid := probe.reply(buffer[:n], fromIP)
		if !valid {
			continue
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		elapsed := time.Since(started)
		if synAck {
			// No TCP connection was established. Explicitly discard the remote
			// half-open handshake as well as Linux's own automatic reset.
			_, _ = packetConn.WriteTo(probe.segment(probe.sequence+1, remoteSequence+1, tcpFlagRST|tcpFlagACK), remote)
		}
		return elapsed, nil
	}
}

// Connecting UDP selects a source address through the same native DIRECT
// socket policy without transmitting a packet or resolving a hostname.
func tcpSYNSource(ctx context.Context, destination netip.Addr, port uint16, options ...dialer.Option) (netip.Addr, error) {
	network := "udp4"
	if destination.Is6() {
		network = "udp6"
	}
	conn, err := dialer.DialContext(ctx, network, netip.AddrPortFrom(destination, port).String(), options...)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("select TCP SYN source address: %w", err)
	}
	defer conn.Close()
	local, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil || local.Addr().IsUnspecified() || local.Addr().Unmap().Is4() != destination.Is4() {
		return netip.Addr{}, fmt.Errorf("invalid TCP SYN source address %s", conn.LocalAddr())
	}
	return local.Addr().Unmap(), nil
}

// Bind an unconnected TCP socket, without listen/connect, to reserve the port.
// The kernel cannot allocate our tuple to an unrelated TCP connection while
// raw replies are in flight; no temporary service is opened on that port.
func reserveTCPSYNPort(source netip.Addr) (int, uint16, error) {
	family := unix.AF_INET
	if source.Is6() {
		family = unix.AF_INET6
	}
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_TCP)
	if err != nil {
		return -1, 0, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = unix.Close(fd)
		}
	}()
	var address unix.Sockaddr
	if source.Is4() {
		address = &unix.SockaddrInet4{Addr: source.As4()}
	} else {
		address6 := &unix.SockaddrInet6{Addr: source.As16()}
		if source.Zone() != "" {
			iface, err := net.InterfaceByName(source.Zone())
			if err != nil {
				return -1, 0, err
			}
			address6.ZoneId = uint32(iface.Index)
		}
		address = address6
	}
	if err := unix.Bind(fd, address); err != nil {
		return -1, 0, err
	}
	bound, err := unix.Getsockname(fd)
	if err != nil {
		return -1, 0, err
	}
	var port int
	switch bound := bound.(type) {
	case *unix.SockaddrInet4:
		port = bound.Port
	case *unix.SockaddrInet6:
		port = bound.Port
	default:
		return -1, 0, errors.New("unexpected TCP SYN source socket family")
	}
	keep = true
	return fd, uint16(port), nil
}

// Source selection and socket setup have not emitted a SYN. A deadline in
// this phase says nothing about the remote IP. Keep real local route failures
// and cancellation, but do not expose a setup timeout as reachability evidence.
func tcpSYNSetupError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
		return err
	}
	var networkError net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &networkError) && networkError.Timeout()) {
		return errDNSProbeNotStarted
	}
	return err
}

func tcpSYNContextError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%s: %w", operation, err)
}
