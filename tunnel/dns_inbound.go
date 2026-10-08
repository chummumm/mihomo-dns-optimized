package tunnel

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/dnsmessage"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

const (
	dnsInboundProbeTimeout = 5 * time.Second
	dnsInboundIdleTimeout  = 60 * time.Second
)

var (
	dnsInboundSessions = make(chan struct{}, 128)
	dnsInboundQueries  = make(chan struct{}, 256)
)

type dnsInboundExchange func(context.Context, []byte, *C.Metadata) ([]byte, error)

// Only forwarded traffic with an original resolver endpoint is classified here.
// Locally served DNS carries its inbound context through the DNS service;
// bootstrap and other INNER connections must not recursively classify themselves.
func dnsInboundCandidate(metadata *C.Metadata) bool {
	return DNSRuleRoutingApplies(metadata) && metadata.Type != C.INNER && metadata.DstPort == 53 &&
		(metadata.Host != "" || metadata.DstIP.IsGlobalUnicast() || metadata.DstIP.IsLoopback() || metadata.DstIP.IsLinkLocalUnicast())
}

func tryHandleDNSUDP(packet C.PacketAdapter) bool {
	return handleDNSInboundUDP(packet, Tunnel.ExchangeDNS)
}

func handleDNSInboundUDP(packet C.PacketAdapter, exchange dnsInboundExchange) bool {
	metadata := packet.Metadata()
	if !dnsInboundCandidate(metadata) {
		return false
	}
	query := packet.Data()
	if _, err := dnsmessage.UnpackQuery(query); err != nil {
		return false
	}
	select {
	case dnsInboundQueries <- struct{}{}:
	default:
		packet.Drop()
		return true
	}
	metadata = metadata.Clone()
	var replyAddr net.Addr = metadata.UDPAddr()
	if metadata.Host != "" {
		// SOCKS and sing datagrams can carry a resolver hostname. Preserve
		// that original address in their response envelope after bootstrap.
		replyAddr = N.NewCustomAddr("udp", metadata.RemoteAddress(), nil)
	}
	go func() {
		defer packet.Drop()
		defer func() { <-dnsInboundQueries }()
		ctx, cancel := context.WithTimeout(context.Background(), dnsProxyTimeout)
		defer cancel()
		response, err := exchange(ctx, query, metadata)
		if err != nil {
			log.Debugln("[DNS routing] UDP query from %s failed: %v", metadata.SourceDetail(), err)
			return
		}
		if len(response) >= 12 && len(response) <= dnsmessage.MaxSize {
			_, _ = packet.WriteBack(response, replyAddr)
		}
	}()
	return true
}

func tryHandleDNSTCP(connCtx C.ConnContext) bool {
	return handleDNSInboundTCP(connCtx, Tunnel.ExchangeDNS)
}

func handleDNSInboundTCP(connCtx C.ConnContext, exchange dnsInboundExchange) bool {
	metadata := connCtx.Metadata()
	if !dnsInboundCandidate(metadata) {
		return false
	}
	conn := connCtx.Conn()
	_ = conn.SetReadDeadline(time.Now().Add(dnsInboundProbeTimeout))
	// Peek never consumes bytes. A failed probe leaves the complete original
	// stream available to the ordinary tunnel, including partial frames.
	defer conn.SetReadDeadline(time.Time{})
	header, err := conn.Peek(2)
	if err != nil {
		return false
	}
	size := int(binary.BigEndian.Uint16(header))
	if size < 12 {
		return false
	}
	header, err = conn.Peek(14)
	if err != nil {
		return false
	}
	// Reject obvious non-DNS without waiting for a length inferred from its
	// first two bytes (for example an HTTP request sent to destination 53).
	flags := binary.BigEndian.Uint16(header[4:6])
	if flags&0xfa0f != 0 || binary.BigEndian.Uint16(header[6:8]) != 1 ||
		binary.BigEndian.Uint16(header[8:10]) != 0 || binary.BigEndian.Uint16(header[10:12]) != 0 {
		return false
	}
	select {
	case dnsInboundSessions <- struct{}{}:
		defer func() { <-dnsInboundSessions }()
	default:
		// The header identifies a DNS candidate; overload must not silently
		// send it through the ordinary connection-based routing path.
		return true
	}
	conn.Grow(size + 2)
	frame, err := conn.Peek(size + 2)
	if err != nil {
		return false
	}
	if _, err := dnsmessage.UnpackQuery(frame[2:]); err != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Time{})
	metadata = metadata.Clone()
	for {
		// A mode/flag change closes an already classified connection. Its
		// client's next connection then follows the newly selected policy.
		if !dnsInboundCandidate(metadata) {
			return true
		}
		var length [2]byte
		_ = conn.SetReadDeadline(time.Now().Add(dnsInboundIdleTimeout))
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return true
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n < 12 {
			return true
		}
		query := make([]byte, n)
		_ = conn.SetReadDeadline(time.Now().Add(dnsInboundProbeTimeout))
		if _, err := io.ReadFull(conn, query); err != nil {
			return true
		}
		if !dnsInboundCandidate(metadata) {
			return true
		}
		select {
		case dnsInboundQueries <- struct{}{}:
		default:
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), dnsProxyTimeout)
		response, err := exchange(ctx, query, metadata)
		cancel()
		<-dnsInboundQueries
		if err != nil {
			log.Debugln("[DNS routing] TCP query from %s failed: %v", metadata.SourceDetail(), err)
			return true
		}
		if len(response) < 12 || len(response) > dnsmessage.MaxSize {
			return true
		}
		binary.BigEndian.PutUint16(length[:], uint16(len(response)))
		_ = conn.SetWriteDeadline(time.Now().Add(dnsProxyTimeout))
		if err := writeDNSProxyFrame(conn, length[:]); err != nil {
			return true
		}
		if err := writeDNSProxyFrame(conn, response); err != nil {
			return true
		}
	}
}
