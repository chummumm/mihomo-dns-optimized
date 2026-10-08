// Package dnsproxy implements a DNS-only mixed-protocol listener.
// It never relays an opaque stream or enters the ordinary UDP NAT table: every
// DNS message is exchanged separately so a persistent client can change routes.
package dnsproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/component/auth"
	"github.com/metacubex/mihomo/component/dnsmessage"
	C "github.com/metacubex/mihomo/constant"
	authStore "github.com/metacubex/mihomo/listener/auth"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/transport/socks4"
	"github.com/metacubex/mihomo/transport/socks5"
)

const (
	maxSessions      = 128
	maxQueries       = 256
	maxDNSMessage    = 65535
	maxUDPPacket     = 65507
	maxHTTPHeader    = 8192
	handshakeTimeout = 5 * time.Second
	queryTimeout     = 5 * time.Second
	idleTimeout      = 60 * time.Second
)

type Listener struct {
	listener  net.Listener
	udp       net.PacketConn
	defaults  bool
	lc        C.InboundListenConfig
	store     auth.AuthStore
	exchanger C.DNSExchanger
	additions []inbound.Addition
	addr      string
	ctx       context.Context
	cancel    context.CancelFunc
	sessions  chan struct{}
	queries   chan struct{}
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	closed    bool
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func New(addr string, lc C.InboundListenConfig, store auth.AuthStore, exchanger C.DNSExchanger, additions ...inbound.Addition) (*Listener, error) {
	return newListener(addr, lc, store, exchanger, false, additions...)
}

// NewDefault opens the top-level DNS-only mixed port, including SOCKS UDP on
// the same port number. It shares the ordinary mixed port's listener settings,
// global authentication, skip-auth prefixes and LAN access controls.
func NewDefault(addr string, tunnel C.Tunnel) (*Listener, error) {
	exchanger, ok := tunnel.(C.DNSExchanger)
	if !ok {
		return nil, errors.New("dns-proxy-port requires a tunnel with DNS exchange support")
	}
	return newListener(addr, inbound.NewListenConfig(), authStore.Default, exchanger, true,
		inbound.WithInName("DEFAULT-DNS-PROXY"), inbound.WithSpecialRules(""))
}

func newListener(addr string, lc C.InboundListenConfig, store auth.AuthStore, exchanger C.DNSExchanger, defaults bool, additions ...inbound.Addition) (*Listener, error) {
	if exchanger == nil {
		return nil, errors.New("dns-proxy requires a DNS exchanger")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		cancel()
		return nil, err
	}
	l := &Listener{
		listener: ln, lc: lc, store: store, exchanger: exchanger, defaults: defaults,
		additions: append([]inbound.Addition(nil), additions...), addr: addr,
		ctx: ctx, cancel: cancel, sessions: make(chan struct{}, maxSessions),
		queries: make(chan struct{}, maxQueries), conns: make(map[net.Conn]struct{}),
	}
	if defaults {
		// Use the actual TCP address so port 0 also shares one port number.
		l.udp, err = lc.ListenPacket(ctx, "udp", ln.Addr().String())
		if err != nil {
			_ = ln.Close()
			cancel()
			return nil, err
		}
		l.wg.Add(1)
		go l.readDefaultUDP()
	}
	l.wg.Add(1)
	go l.accept()
	return l, nil
}

func (l *Listener) RawAddress() string { return l.addr }
func (l *Listener) Address() string    { return l.listener.Addr().String() }

func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		l.mu.Lock()
		l.closed = true
		l.closeErr = l.listener.Close()
		if l.udp != nil {
			l.closeErr = errors.Join(l.closeErr, l.udp.Close())
		}
		for conn := range l.conns {
			_ = conn.Close()
		}
		l.mu.Unlock()
	})
	l.wg.Wait()
	return l.closeErr
}

func (l *Listener) accept() {
	defer l.wg.Done()
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			if l.ctx.Err() != nil {
				return
			}
			// Avoid spinning on a temporary resource shortage.
			select {
			case <-l.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if l.defaults && !inbound.IsRemoteAddrDisAllowed(conn.RemoteAddr()) {
			_ = conn.Close()
			continue
		}
		select {
		case l.sessions <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			<-l.sessions
			_ = conn.Close()
			return
		}
		l.conns[conn] = struct{}{}
		l.wg.Add(1)
		l.mu.Unlock()
		go func() {
			defer l.wg.Done()
			defer func() {
				_ = conn.Close()
				l.mu.Lock()
				delete(l.conns, conn)
				l.mu.Unlock()
				<-l.sessions
			}()
			l.handle(conn)
		}()
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

func (l *Listener) handle(raw net.Conn) {
	_ = raw.SetDeadline(time.Now().Add(handshakeTimeout))
	conn := &bufferedConn{Conn: raw, reader: bufio.NewReader(raw)}
	head, err := conn.reader.Peek(1)
	if err != nil {
		return
	}
	switch head[0] {
	case socks4.Version:
		l.handleSOCKS4(conn)
	case socks5.Version:
		l.handleSOCKS(conn)
	default:
		l.handleHTTP(conn)
	}
}

// Only literal resolver addresses are accepted. Resolving a resolver hostname
// here could recurse through SmartDNS back into this listener.
func dnsTarget(target socks5.Addr) (*C.Metadata, error) {
	addr := target.UDPAddr()
	if addr == nil {
		return nil, socks5.ErrAddressNotSupported
	}
	ip, ok := netip.AddrFromSlice(addr.IP)
	if !ok || !(ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return nil, socks5.ErrAddressNotSupported
	}
	if addr.Port != 53 {
		return nil, socks5.ErrConnectionNotAllowed
	}
	return &C.Metadata{DstIP: ip.Unmap(), DstPort: 53}, nil
}

func (l *Listener) metadata(target *C.Metadata, source, local net.Addr, network C.NetWork, kind C.Type, user string) *C.Metadata {
	m := *target
	m.NetWork, m.Type = network, kind
	inbound.ApplyAdditions(&m, inbound.WithSrcAddr(source), inbound.WithInAddr(local))
	inbound.ApplyAdditions(&m, l.additions...)
	m.InUser = user
	return &m
}

func (l *Listener) authenticator(source net.Addr) auth.Authenticator {
	if l.defaults && inbound.SkipAuthRemoteAddr(source) {
		return nil
	}
	if l.store == nil {
		return nil
	}
	return l.store.Authenticator()
}

func (l *Listener) handleSOCKS4(conn *bufferedConn) {
	var target *C.Metadata
	_, _, user, err := socks4.ServerHandshakeWithHandler(conn, l.authenticator(conn.RemoteAddr()), func(addr string, _ socks4.Command, _ string) error {
		var err error
		// SOCKS4a may encode a literal IPv4/IPv6 address as its host field.
		// Resolver hostnames remain disallowed to avoid DNS bootstrap loops.
		target, err = dnsTarget(socks5.ParseAddr(addr))
		return err
	})
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	l.serveTCP(conn, l.metadata(target, conn.RemoteAddr(), conn.LocalAddr(), C.TCP, C.SOCKS4, user))
}

func (l *Listener) handleSOCKS(conn *bufferedConn) {
	var udp net.PacketConn
	var target *C.Metadata
	var clientPort uint16
	defer func() {
		if udp != nil {
			_ = udp.Close()
		}
	}()
	_, command, user, err := socks5.ServerHandshakeWithHandler(conn, l.authenticator(conn.RemoteAddr()), func(addr socks5.Addr, command socks5.Command, _ string) (net.Addr, error) {
		if command == socks5.CmdConnect {
			var err error
			target, err = dnsTarget(addr)
			return conn.LocalAddr(), err
		}
		// UDP ASSOCIATE's address describes the client's UDP endpoint, not a
		// DNS resolver. Restrict it to the TCP peer and pin its port below.
		client := addr.UDPAddr()
		peer, err := netip.ParseAddrPort(conn.RemoteAddr().String())
		if client == nil || err != nil {
			return nil, socks5.ErrAddressNotSupported
		}
		ip, ok := netip.AddrFromSlice(client.IP)
		if !ok || (!ip.IsUnspecified() && ip.Unmap() != peer.Addr().Unmap()) {
			return nil, socks5.ErrConnectionNotAllowed
		}
		// SmartDNS sends an unspecified client address with the resolver's
		// port (53), then creates its UDP socket on an ephemeral local port.
		// Treat an unspecified address as an unknown endpoint and pin the
		// first valid packet from the authenticated TCP peer instead.
		if !ip.IsUnspecified() {
			clientPort = uint16(client.Port)
		}
		local, err := netip.ParseAddrPort(conn.LocalAddr().String())
		if err != nil {
			return nil, socks5.ErrAddressNotSupported
		}
		udp, err = l.lc.ListenPacket(l.ctx, "udp", net.JoinHostPort(local.Addr().String(), "0"))
		if err != nil {
			return nil, err
		}
		return udp.LocalAddr(), nil
	})
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if command == socks5.CmdUDPAssociate {
		l.serveUDP(conn, udp, user, clientPort)
		return
	}
	l.serveTCP(conn, l.metadata(target, conn.RemoteAddr(), conn.LocalAddr(), C.TCP, C.SOCKS5, user))
}

func (l *Listener) handleHTTP(conn *bufferedConn) {
	// Read only a bounded header. The original reader keeps any pipelined
	// DNS bytes, which must not be lost when switching to framed DNS/TCP.
	var header bytes.Buffer
	for {
		line, err := conn.reader.ReadSlice('\n')
		if err != nil || header.Len()+len(line) > maxHTTPHeader {
			writeHTTPStatus(conn, http.StatusRequestHeaderFieldsTooLarge, false)
			return
		}
		header.Write(line)
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			break
		}
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header.Bytes())))
	if err != nil || req.Method != http.MethodConnect || req.ContentLength > 0 || len(req.TransferEncoding) != 0 {
		writeHTTPStatus(conn, http.StatusMethodNotAllowed, false)
		return
	}
	user, pass, ok := basicProxyAuth(req.Header.Get("Proxy-Authorization"))
	if authenticator := l.authenticator(conn.RemoteAddr()); authenticator != nil && (!ok || !authenticator.Verify(user, pass)) {
		writeHTTPStatus(conn, http.StatusProxyAuthRequired, true)
		return
	}
	target, err := dnsTarget(socks5.ParseAddr(req.Host))
	if err != nil {
		writeHTTPStatus(conn, http.StatusForbidden, false)
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	l.serveTCP(conn, l.metadata(target, conn.RemoteAddr(), conn.LocalAddr(), C.TCP, C.HTTPS, user))
}

func basicProxyAuth(header string) (user, pass string, ok bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	plain, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(plain), ":")
	return
}

func writeHTTPStatus(conn net.Conn, code int, authRequired bool) {
	var extra string
	if authRequired {
		extra = "Proxy-Authenticate: Basic realm=\"dns-proxy\"\r\n"
	}
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n%sContent-Length: 0\r\nConnection: close\r\n\r\n", code, http.StatusText(code), extra)
}

func (l *Listener) exchange(ctx context.Context, query []byte, metadata *C.Metadata) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return l.exchanger.ExchangeDNS(ctx, query, metadata)
}

func (l *Listener) serveTCP(conn net.Conn, metadata *C.Metadata) {
	var size [2]byte
	for {
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(size[:]))
		if n < 12 || n > maxDNSMessage {
			return
		}
		query := make([]byte, n)
		// A partial frame cannot hold a client slot for an idle period.
		_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		select {
		case l.queries <- struct{}{}:
		default:
			return
		}
		response, err := l.exchange(l.ctx, query, metadata)
		<-l.queries
		if err != nil {
			log.Debugln("[DNS proxy] TCP query from %s failed: %v", conn.RemoteAddr(), err)
			return
		}
		if len(response) < 12 || len(response) > maxDNSMessage {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(response)))
		_ = conn.SetWriteDeadline(time.Now().Add(queryTimeout))
		buffers := net.Buffers{size[:], response}
		if _, err := buffers.WriteTo(conn); err != nil {
			return
		}
	}
}

func (l *Listener) serveUDP(conn net.Conn, udp net.PacketConn, user string, clientPort uint16) {
	ctx, cancel := context.WithCancel(l.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close() // also end the control connection after UDP idle/error
		l.readUDP(ctx, cancel, conn, udp, user, clientPort)
	}()
	// Closing the authenticated control channel closes the association and
	// cancels outstanding DNS exchanges; no unauthenticated permanent relay.
	_, _ = io.Copy(io.Discard, conn)
	cancel()
	_ = udp.Close()
	<-done
}

// The ordinary mixed port also accepts SOCKS5 UDP envelopes sent directly to
// its configured port. These packets have no authentication fields: unlike an
// authenticated association, this compatibility path must not bypass global
// authentication. It is usable without auth, or from a skip-auth prefix.
func (l *Listener) readDefaultUDP() {
	defer l.wg.Done()
	var queries sync.WaitGroup
	defer queries.Wait()
	buf := make([]byte, maxDNSMessage+1)
	for {
		n, source, err := l.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if !inbound.IsRemoteAddrDisAllowed(source) || l.authenticator(source) != nil {
			continue
		}
		if n > maxUDPPacket {
			continue
		}
		target, query, err := socks5.DecodeUDPPacket(buf[:n])
		if err != nil || len(query) < 12 || len(query) > maxDNSMessage {
			continue
		}
		metadata, err := dnsTarget(target)
		if err != nil {
			continue
		}
		metadata = l.metadata(metadata, source, l.udp.LocalAddr(), C.UDP, C.SOCKS5, "")
		l.enqueueUDP(l.ctx, &queries, l.udp, target, query, metadata, source)
	}
}

func (l *Listener) enqueueUDP(ctx context.Context, queries *sync.WaitGroup, udp net.PacketConn, target socks5.Addr, query []byte, metadata *C.Metadata, source net.Addr) {
	select {
	case l.queries <- struct{}{}:
	default:
		return
	}
	query = bytes.Clone(query)
	target = bytes.Clone(target)
	queries.Add(1)
	go func() {
		defer queries.Done()
		defer func() { <-l.queries }()
		response, err := l.exchange(ctx, query, metadata)
		if err != nil {
			log.Debugln("[DNS proxy] UDP query from %s failed: %v", source, err)
			return
		}
		if len(response) < 12 || 3+len(target)+len(response) > maxUDPPacket {
			return
		}
		packet, err := socks5.EncodeUDPPacket(target, response)
		if err == nil && ctx.Err() == nil {
			_ = udp.SetWriteDeadline(time.Now().Add(queryTimeout))
			_, _ = udp.WriteTo(packet, source)
		}
	}()
}

func (l *Listener) readUDP(ctx context.Context, cancel context.CancelFunc, conn net.Conn, udp net.PacketConn, user string, clientPort uint16) {
	var queries sync.WaitGroup
	defer queries.Wait()
	defer cancel()
	peer, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		return
	}
	buf := make([]byte, maxDNSMessage+1)
	_ = udp.SetReadDeadline(time.Now().Add(idleTimeout))
	for {
		n, source, err := udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if l.defaults && !inbound.IsRemoteAddrDisAllowed(source) {
			continue
		}
		sender, err := netip.ParseAddrPort(source.String())
		if err != nil || sender.Addr().Unmap() != peer.Addr().Unmap() || (clientPort != 0 && sender.Port() != clientPort) {
			continue
		}
		if n > maxUDPPacket {
			continue
		}
		target, query, err := socks5.DecodeUDPPacket(buf[:n])
		if err != nil || len(query) < 12 || len(query) > maxDNSMessage {
			continue
		}
		metadata, err := dnsTarget(target)
		if err != nil {
			continue
		}
		// A malformed first DNS message must not capture the association's
		// source port or refresh its idle timer. Share the core's query parser
		// here; subsequent packets are still validated by the exchanger.
		if clientPort == 0 {
			if _, err := dnsmessage.UnpackQuery(query); err != nil {
				continue
			}
			clientPort = sender.Port()
		}
		_ = udp.SetReadDeadline(time.Now().Add(idleTimeout))
		metadata = l.metadata(metadata, source, conn.LocalAddr(), C.UDP, C.SOCKS5, user)
		l.enqueueUDP(ctx, &queries, udp, target, query, metadata, source)
	}
}

var _ C.Listener = (*Listener)(nil)
