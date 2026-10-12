package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"time"
)

var errTCPSYNUnavailable = errors.New("TCP SYN speed checking is unavailable")

// A refusal is a TCP response with a usable network RTT. It says that the
// selected port is closed, not that the destination IP cannot be reached.
func measureTCPConnect(ctx context.Context, connect func(context.Context) (net.Conn, error)) (time.Duration, error) {
	started := time.Now()
	conn, err := connect(ctx)
	elapsed := time.Since(started)
	if conn != nil {
		_ = conn.Close()
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil && !errors.Is(err, syscall.ECONNREFUSED) {
		return 0, err
	}
	return elapsed, nil
}

const (
	tcpProbeHeaderSize = 20
	tcpFlagFIN         = 0x01
	tcpFlagSYN         = 0x02
	tcpFlagRST         = 0x04
	tcpFlagPSH         = 0x08
	tcpFlagACK         = 0x10
	tcpFlagURG         = 0x20
)

// The tuple and unpredictable sequence number belong to one bounded probe.
// Replies must acknowledge exactly that SYN; an unrelated RST is not evidence.
type tcpSYNProbe struct {
	source, destination netip.AddrPort
	sequence            uint32
}

func (p tcpSYNProbe) segment(sequence, acknowledgement uint32, flags byte) []byte {
	segment := make([]byte, tcpProbeHeaderSize)
	binary.BigEndian.PutUint16(segment[0:2], p.source.Port())
	binary.BigEndian.PutUint16(segment[2:4], p.destination.Port())
	binary.BigEndian.PutUint32(segment[4:8], sequence)
	binary.BigEndian.PutUint32(segment[8:12], acknowledgement)
	segment[12], segment[13] = 5<<4, flags
	binary.BigEndian.PutUint16(segment[14:16], 65535)
	binary.BigEndian.PutUint16(segment[16:18], tcpProbeChecksum(p.source.Addr(), p.destination.Addr(), segment))
	return segment
}

func (p tcpSYNProbe) request() []byte {
	return p.segment(p.sequence, 0, tcpFlagSYN)
}

// A remote sequence number is not predictable. Its association with this
// transaction is proved by the full tuple, checksum, and ACK of our random ISN.
// Return it only after validation, to form a closing RST for a SYN-ACK.
func (p tcpSYNProbe) reply(segment []byte, source netip.Addr) (remoteSequence uint32, synAck bool, valid bool) {
	if source.Unmap().WithZone("") != p.destination.Addr().Unmap().WithZone("") || len(segment) < tcpProbeHeaderSize {
		return 0, false, false
	}
	headerSize := int(segment[12]>>4) * 4
	if headerSize < tcpProbeHeaderSize || headerSize > len(segment) ||
		binary.BigEndian.Uint16(segment[0:2]) != p.destination.Port() ||
		binary.BigEndian.Uint16(segment[2:4]) != p.source.Port() ||
		binary.BigEndian.Uint32(segment[8:12]) != p.sequence+1 {
		return 0, false, false
	}
	flags := segment[13]
	synAck = flags&tcpFlagSYN != 0
	rst := flags&tcpFlagRST != 0
	if flags&tcpFlagACK == 0 || synAck == rst || flags&(tcpFlagFIN|tcpFlagPSH|tcpFlagURG) != 0 {
		return 0, false, false
	}
	if tcpProbeChecksum(p.destination.Addr(), p.source.Addr(), segment) != 0 {
		return 0, false, false
	}
	return binary.BigEndian.Uint32(segment[4:8]), synAck, true
}

func tcpProbeChecksum(source, destination netip.Addr, segment []byte) uint16 {
	var sum uint32
	add := func(value []byte) {
		for len(value) >= 2 {
			sum += uint32(binary.BigEndian.Uint16(value[:2]))
			value = value[2:]
		}
		if len(value) != 0 {
			sum += uint32(value[0]) << 8
		}
	}
	source, destination = source.Unmap(), destination.Unmap()
	if source.Is4() && destination.Is4() {
		src, dst := source.As4(), destination.As4()
		add(src[:])
		add(dst[:])
	} else if source.Is6() && destination.Is6() {
		src, dst := source.As16(), destination.As16()
		add(src[:])
		add(dst[:])
		sum += uint32(len(segment)) >> 16
	} else {
		return 0xffff
	}
	sum += uint32(len(segment)) & 0xffff
	sum += 6 // IPPROTO_TCP in the IPv4/IPv6 pseudo-header.
	add(segment)
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
