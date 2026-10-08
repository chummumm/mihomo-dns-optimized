package sing_tun

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	icontext "github.com/metacubex/mihomo/context"
	"github.com/metacubex/mihomo/listener/sing"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/sing/common/buf"
	"github.com/metacubex/sing/common/bufio"
	M "github.com/metacubex/sing/common/metadata"
	"github.com/metacubex/sing/common/network"
)

func (h *ListenerHandler) ShouldHijackDns(targetAddr netip.AddrPort) bool {
	for _, addrPort := range h.DnsAddrPorts {
		if addrPort == targetAddr || (addrPort.Addr().IsUnspecified() && targetAddr.Port() == 53) {
			return true
		}
	}
	return false
}

func (h *ListenerHandler) NewConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	if h.ShouldHijackDns(metadata.Destination.AddrPort()) {
		log.Debugln("[DNS] hijack tcp:%s", metadata.Destination.String())
		ctx = h.dnsRoutingContext(ctx, metadata.Source, metadata.Destination, conn.LocalAddr(), C.TCP)
		return resolver.RelayDnsConn(ctx, conn, resolver.DefaultDnsReadTimeout)
	}
	return h.ListenerHandler.NewConnection(ctx, conn, metadata)
}

func (h *ListenerHandler) NewPacket(ctx context.Context, key netip.AddrPort, buffer *buf.Buffer, metadata M.Metadata, init func(natConn network.PacketConn) network.PacketWriter) {
	if h.ShouldHijackDns(metadata.Destination.AddrPort()) {
		log.Debugln("[DNS] hijack udp:%s from %s", metadata.Destination.String(), metadata.Source.String())
		writer := init(nil)
		var local net.Addr
		if addressed, ok := writer.(interface{ LocalAddr() net.Addr }); ok {
			local = addressed.LocalAddr()
		}
		ctx = h.dnsRoutingContext(ctx, metadata.Source, metadata.Destination, local, C.UDP)
		rwOptions := network.ReadWaitOptions{
			FrontHeadroom: network.CalculateFrontHeadroom(writer),
			RearHeadroom:  network.CalculateRearHeadroom(writer),
			MTU:           resolver.SafeDnsPacketSize,
		}
		go relayDnsPacket(ctx, buffer, rwOptions, metadata.Destination, nil, &writer)
		return
	}
	h.ListenerHandler.NewPacket(ctx, key, buffer, metadata, init)
}

func (h *ListenerHandler) NewPacketConnection(ctx context.Context, conn network.PacketConn, metadata M.Metadata) error {
	if h.ShouldHijackDns(metadata.Destination.AddrPort()) {
		log.Debugln("[DNS] hijack udp:%s from %s", metadata.Destination.String(), metadata.Source.String())
		defer func() { _ = conn.Close() }()
		mutex := sync.Mutex{}
		var writer network.PacketWriter = conn // a new interface to set nil in defer
		defer func() {
			mutex.Lock() // this goroutine must exit after all conn.WritePacket() is not running
			defer mutex.Unlock()
			writer = nil
		}()
		rwOptions := network.ReadWaitOptions{
			FrontHeadroom: network.CalculateFrontHeadroom(conn),
			RearHeadroom:  network.CalculateRearHeadroom(conn),
			MTU:           resolver.SafeDnsPacketSize,
		}
		readWaiter, isReadWaiter := bufio.CreatePacketReadWaiter(conn)
		if isReadWaiter {
			readWaiter.InitializeReadWaiter(rwOptions)
		}
		for {
			var (
				readBuff *buf.Buffer
				dest     M.Socksaddr
				err      error
			)
			_ = conn.SetReadDeadline(time.Now().Add(resolver.DefaultDnsReadTimeout))
			readBuff = nil // clear last loop status, avoid repeat release
			if isReadWaiter {
				readBuff, dest, err = readWaiter.WaitReadPacket()
			} else {
				readBuff = rwOptions.NewPacketBuffer()
				dest, err = conn.ReadPacket(readBuff)
				if readBuff != nil {
					rwOptions.PostReturn(readBuff)
				}
			}
			if err != nil {
				if readBuff != nil {
					readBuff.Release()
				}
				if sing.ShouldIgnorePacketError(err) {
					break
				}
				return err
			}
			queryCtx := h.dnsRoutingContext(ctx, metadata.Source, dest, conn.LocalAddr(), C.UDP)
			go relayDnsPacket(queryCtx, readBuff, rwOptions, dest, &mutex, &writer)
		}
		return nil
	}
	return h.ListenerHandler.NewPacketConnection(ctx, conn, metadata)
}

// Hijacked DNS remains a local resolver service, including the synthetic TUN
// gateway addresses. Carry its real inbound policy and source to the service;
// the service chooses an actual upstream before applying DNS rule routing.
func (h *ListenerHandler) dnsRoutingContext(ctx context.Context, source, destination M.Socksaddr, local net.Addr, network C.NetWork) context.Context {
	metadata := &C.Metadata{Type: h.Type, NetWork: network}
	inbound.ApplyAdditions(metadata, inbound.WithSrcAddr(source), inbound.WithDstAddr(destination))
	if local != nil {
		inbound.ApplyAdditions(metadata, inbound.WithInAddr(local))
	}
	inbound.ApplyAdditions(metadata, h.Additions...)
	inbound.ApplyAdditions(metadata, sing.AdditionsFromContext(ctx)...)
	return icontext.WithDNSRoutingInbound(icontext.WithDNSRoutingMetadata(ctx, metadata))
}

func relayDnsPacket(ctx context.Context, readBuff *buf.Buffer, rwOptions network.ReadWaitOptions, dest M.Socksaddr, mutex *sync.Mutex, writer *network.PacketWriter) {
	ctx, cancel := context.WithTimeout(ctx, resolver.DefaultDnsRelayTimeout)
	defer cancel()
	inData := readBuff.Bytes()
	writeBuff := readBuff
	writeBuff.Resize(writeBuff.Start(), 0)
	if len(writeBuff.FreeBytes()) < resolver.SafeDnsPacketSize { // only create a new buffer when space don't enough
		writeBuff = rwOptions.NewPacketBuffer()
	}
	msg, err := resolver.RelayDnsPacket(ctx, inData, writeBuff.FreeBytes())
	if writeBuff != readBuff {
		readBuff.Release()
	}
	if err != nil {
		writeBuff.Release()
		return
	}
	writeBuff.Truncate(len(msg))
	if mutex != nil {
		mutex.Lock()
		defer mutex.Unlock()
	}
	conn := *writer
	if conn == nil {
		writeBuff.Release()
		return
	}
	err = conn.WritePacket(writeBuff, dest) // WritePacket will release writeBuff
	if err != nil {
		writeBuff.Release()
		return
	}
}

func (h *ListenerHandler) TypeMutation(typ C.Type) *ListenerHandler {
	handle := *h
	handle.ListenerHandler = h.ListenerHandler.TypeMutation(typ)
	return &handle
}
