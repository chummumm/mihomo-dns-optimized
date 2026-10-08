package constant

import (
	"context"
	"net"
)

type Tunnel interface {
	// HandleTCPConn will handle a tcp connection blocking
	HandleTCPConn(conn net.Conn, metadata *Metadata)
	// HandleUDPPacket will handle a udp packet nonblocking
	HandleUDPPacket(packet UDPPacket, metadata *Metadata)
	// NatTable return nat table
	NatTable() NatTable
}

// DNSExchanger exchanges detected DNS messages from ordinary inbounds. Each call
// exchanges one wire-format DNS message using resolverMetadata as the transport
// destination; routing is selected independently for every query's question.
// Keeping it separate from Tunnel leaves existing inbounds and tunnel adapters
// unchanged.
type DNSExchanger interface {
	ExchangeDNS(ctx context.Context, query []byte, resolverMetadata *Metadata) ([]byte, error)
}
